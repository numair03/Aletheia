// Package node is the full node: it holds the chain (a core.Ledger), the
// mempool, and the miner, assembles and mines blocks, accepts blocks from
// peers, and serves headers, blocks, locators and Merkle proofs.
//
// Only this package appends to a ledger, and always after
// consensus.CheckHeader (DECISIONS DR-10).
//
// Concurrency. Every exported method is safe for concurrent use. Two locks:
//   - chainMu serialises every change of the chain: accepting a block on the
//     tip, and replacing the chain after a replay (chain.go). It is held for
//     the whole change, including a replay, so a replay reads its own old
//     blocks from a ledger nobody else can change;
//   - mu guards the ledger pointer. Readers hold it shared for a whole
//     method, so a method sees one ledger even across several C calls, and
//     a ledger is never closed under a reader. A writer holds it exclusively
//     only for the change itself (Append, or the pointer swap) together with
//     the mempool reconciliation, so the miner's (tip, batch) snapshot, taken
//     under the shared lock, is always consistent.
//
// Lock order: chainMu, then mu, then the mempool's lock, then the ledger's
// and fee queue's own locks inside their methods. No lock is held while the
// Broadcaster is called, and no method takes mu twice.
package node

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Anon-HP/Aletheia/internal/consensus"
	"github.com/Anon-HP/Aletheia/internal/core"
	"github.com/Anon-HP/Aletheia/internal/mempool"
	"github.com/Anon-HP/Aletheia/internal/miner"
)

var (
	// ErrStopped is returned by methods that need the chain after Stop.
	ErrStopped = errors.New("node: stopped")
	// ErrNoPeer: the node has no Broadcaster to fetch blocks from.
	ErrNoPeer = errors.New("node: no peer to fetch from")
	// errNotOnTip: the block does not build on the current tip.
	errNotOnTip = errors.New("node: block does not extend the tip")
	// errTipChanged: the tip moved while a block was being mined.
	errTipChanged = errors.New("node: tip changed during mining")
)

// idleWait bounds how long the mining loop waits before retrying when it
// cannot make progress (an error, or no transaction fits on the tip).
const idleWait = 100 * time.Millisecond

// Options are the node's local settings (not network parameters).
type Options struct {
	// Workers is the number of mining goroutines; < 1 means runtime.NumCPU().
	Workers int
	// Logf, if set, receives one line per chain event (block mined,
	// accepted, chain replaced).
	Logf func(format string, args ...any)
}

// ProofResult is a Merkle inclusion proof for a confirmed transaction. It
// carries no root: the client verifies against the Merkle root in its own
// header at Height (M-R8).
type ProofResult struct {
	Height uint64
	Tx     core.Tx
	Proof  core.Proof
}

// Node is a full node. Create it with New; Start runs the miner; Stop ends
// the miner and frees the chain and the mempool.
type Node struct {
	p    consensus.Params
	b    Broadcaster
	opts Options

	chainMu sync.Mutex    // serialises chain changes (see the package comment)
	mu      sync.RWMutex  // guards ledger and tipCh
	ledger  *core.Ledger  // nil after Stop
	tipCh   chan struct{} // closed and replaced whenever the tip changes

	pool *mempool.Pool

	runMu   sync.Mutex // guards cancel and started
	cancel  context.CancelFunc
	started bool
	done    sync.WaitGroup // the mining loop
}

// New creates a node holding only the genesis block of p. b may be nil for a
// node without peers.
func New(p consensus.Params, b Broadcaster, opts Options) (*Node, error) {
	if p.MaxTxPerBlock < 1 || p.Difficulty > core.MaxDifficulty {
		return nil, core.ErrArg
	}
	if b == nil {
		b = noPeers{}
	}
	p.Allocations = append([]core.Alloc(nil), p.Allocations...) // own copy
	l, err := core.NewLedger(p.Genesis, p.Allocations)
	if err != nil {
		return nil, err
	}
	pool, err := mempool.New(l)
	if err != nil {
		l.Close()
		return nil, err
	}
	return &Node{p: p, b: b, opts: opts, ledger: l, tipCh: make(chan struct{}), pool: pool}, nil
}

func (n *Node) logf(format string, args ...any) {
	if n.opts.Logf != nil {
		n.opts.Logf(format, args...)
	}
}

// view runs f on the current ledger under the shared lock.
func (n *Node) view(f func(l *core.Ledger) error) error {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.ledger == nil {
		return ErrStopped
	}
	return f(n.ledger)
}

// tipChanged wakes everyone waiting on the old tip. Caller holds mu
// exclusively.
func (n *Node) tipChanged() {
	close(n.tipCh)
	n.tipCh = make(chan struct{})
}

// hashAt returns the block hash at height h of l, for locators. Callers pass
// only heights <= l's tip while holding mu, so HeaderAt cannot fail.
func hashAt(l *core.Ledger) func(uint64) core.Hash {
	return func(h uint64) core.Hash {
		hd, err := l.HeaderAt(h)
		if err != nil {
			return core.Hash{}
		}
		return core.HeaderHash(hd)
	}
}

// ---- Lifecycle ----

// Start runs the mining loop until ctx is done or Stop is called. A second
// Start, or a Start after Stop, does nothing.
func (n *Node) Start(ctx context.Context) {
	n.runMu.Lock()
	defer n.runMu.Unlock()
	if n.started {
		return
	}
	n.started = true
	ctx, n.cancel = context.WithCancel(ctx)
	n.done.Add(1)
	go func() {
		defer n.done.Done()
		n.mineLoop(ctx)
	}()
}

// Stop ends the mining loop, waits for it, then frees the mempool and the
// ledger. Afterwards reads return zero values and writes ErrStopped (or
// mempool.ErrClosed). Stop is idempotent and works without Start.
func (n *Node) Stop() {
	n.runMu.Lock()
	n.started = true // no Start after Stop
	if n.cancel != nil {
		n.cancel()
	}
	n.runMu.Unlock()
	n.done.Wait()

	n.chainMu.Lock()
	defer n.chainMu.Unlock()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ledger == nil {
		return
	}
	n.pool.Close()
	n.ledger.Close()
	n.ledger = nil
	n.tipChanged()
}

// ---- Reads ----

// Tip returns the tip's height and header ((0, zero header) after Stop).
func (n *Node) Tip() (uint64, core.Header) {
	var h uint64
	var hd core.Header
	n.view(func(l *core.Ledger) error {
		var err error
		h, hd, err = l.Tip()
		return err
	})
	return h, hd
}

// Headers returns the headers from height from up to the tip (none if from
// is above the tip).
func (n *Node) Headers(from uint64) []core.Header {
	var out []core.Header
	n.view(func(l *core.Ledger) error {
		tip, err := l.Height()
		if err != nil {
			return err
		}
		out, err = headersRange(l, from, tip)
		return err
	})
	return out
}

// Blocks returns the blocks from height from up to the tip (none if from is
// above the tip). Genesis has no transactions.
func (n *Node) Blocks(from uint64) []core.Block {
	var out []core.Block
	n.view(func(l *core.Ledger) error {
		tip, err := l.Height()
		if err != nil {
			return err
		}
		out, err = blocksRange(l, from, tip)
		return err
	})
	return out
}

func headersRange(l *core.Ledger, from, to uint64) ([]core.Header, error) {
	var out []core.Header
	for h := from; h <= to; h++ {
		hd, err := l.HeaderAt(h)
		if err != nil {
			return nil, err
		}
		out = append(out, hd)
	}
	return out, nil
}

func blocksRange(l *core.Ledger, from, to uint64) ([]core.Block, error) {
	var out []core.Block
	for h := from; h <= to; h++ {
		hd, err := l.HeaderAt(h)
		if err != nil {
			return nil, err
		}
		txs, err := l.BlockTxs(h)
		if err != nil {
			return nil, err
		}
		out = append(out, core.Block{Header: hd, Txs: txs})
	}
	return out, nil
}

// Locator returns the block locator of the node's chain (DR-14): heights
// tip, tip-1, ..., then exponentially sparser, ending at genesis. Nil after
// Stop.
func (n *Node) Locator() []consensus.LocatorEntry {
	var loc []consensus.LocatorEntry
	n.view(func(l *core.Ledger) error {
		tip, err := l.Height()
		if err != nil {
			return err
		}
		loc = consensus.BuildLocator(tip, hashAt(l))
		return nil
	})
	return loc
}

// forkOf resolves a requester's locator against l. Caller holds mu.
func forkOf(l *core.Ledger, loc []consensus.LocatorEntry) (fork, tip uint64, err error) {
	tip, err = l.Height()
	if err != nil {
		return 0, 0, err
	}
	fork, err = consensus.ForkPoint(loc, tip, hashAt(l))
	return fork, tip, err
}

// HeadersAfter resolves a requester's locator to the fork point (the highest
// locator height at which the chains agree) and returns the headers after
// it, up to the tip. Errors wrap consensus.ErrBadLocator or are
// consensus.ErrGenesisMismatch.
func (n *Node) HeadersAfter(loc []consensus.LocatorEntry) (fork uint64, hs []core.Header, err error) {
	err = n.view(func(l *core.Ledger) error {
		var tip uint64
		var err error
		if fork, tip, err = forkOf(l, loc); err != nil {
			return err
		}
		hs, err = headersRange(l, fork+1, tip)
		return err
	})
	return fork, hs, err
}

// BlocksAfter is HeadersAfter for whole blocks: the answer to a peer's
// FetchBlocks.
func (n *Node) BlocksAfter(loc []consensus.LocatorEntry) (fork uint64, bs []core.Block, err error) {
	err = n.view(func(l *core.Ledger) error {
		var tip uint64
		var err error
		if fork, tip, err = forkOf(l, loc); err != nil {
			return err
		}
		bs, err = blocksRange(l, fork+1, tip)
		return err
	})
	return fork, bs, err
}

// Proof returns the inclusion proof of a confirmed transaction;
// core.ErrNotFound if it is not in the chain.
func (n *Node) Proof(txid core.Hash) (ProofResult, error) {
	var r ProofResult
	err := n.view(func(l *core.Ledger) error {
		h, idx, err := l.FindTx(txid)
		if err != nil {
			return err
		}
		txs, err := l.BlockTxs(h)
		if err != nil {
			return err
		}
		proof, err := core.MerkleProof(core.TxIDs(txs), uint64(idx))
		if err != nil {
			return err
		}
		r = ProofResult{Height: h, Tx: txs[idx], Proof: proof}
		return nil
	})
	return r, err
}

// Account returns an account's balance and next sequence number in the
// tip's state ((0, 0) for an unknown account, or after Stop).
func (n *Node) Account(id core.Hash) (balance, nextSeq uint64) {
	n.view(func(l *core.Ledger) error {
		var err error
		balance, nextSeq, err = l.Account(id)
		return err
	})
	return balance, nextSeq
}

// MempoolSize is the number of pending transactions: O(1) and lock-free.
// Informational only; routing must not use it (CD-29 item 2).
func (n *Node) MempoolSize() int { return n.pool.Len() }

// Mempool returns the pending transactions in fee order (nil after Stop).
func (n *Node) Mempool() []core.Tx {
	txs, _ := n.pool.Pending()
	return txs
}

// SubmitTx admits tx to the mempool and returns its txid. Rejections:
// mempool.ErrDuplicate, mempool.ErrSenderPending, a ledger rule error
// (core.ErrBalance, core.ErrSeq, core.ErrOverflow), or mempool.ErrClosed
// after Stop.
func (n *Node) SubmitTx(tx core.Tx) (core.Hash, error) {
	return n.pool.Submit(tx)
}

// ---- Accepting a block on the tip ----

// acceptOnTip appends b if it builds on the current tip: consensus.CheckHeader
// (linkage, fixed difficulty, proof of work, hash recomputed), then
// Ledger.Append (Merkle root, transaction set, balances, sequence numbers),
// then mempool reconciliation (confirmed and now-invalid transactions
// evicted). Returns errNotOnTip, without checking anything else, if b.Prev
// is not the tip's hash; a validation error leaves everything unchanged.
func (n *Node) acceptOnTip(b core.Block) error {
	n.chainMu.Lock()
	defer n.chainMu.Unlock()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ledger == nil {
		return ErrStopped
	}
	h, tip, err := n.ledger.Tip()
	if err != nil {
		return err
	}
	if b.Header.Prev != core.HeaderHash(tip) {
		return errNotOnTip
	}
	if err := consensus.CheckHeader(n.p, tip, b.Header); err != nil {
		return fmt.Errorf("node: block %d rejected: %w", h+1, err)
	}
	if err := n.ledger.Append(b); err != nil {
		return fmt.Errorf("node: block %d rejected: %w", h+1, err)
	}
	if _, _, err := n.pool.Reconcile(n.ledger, nil); err != nil {
		n.logf("mempool reconcile after block %d: %v", h+1, err)
	}
	n.tipChanged()
	return nil
}

// ---- Block assembly and mining ----

// assemble builds the next block on the current tip: up to MaxTxPerBlock
// pending transactions in fee order, each kept only if the kept list still
// passes Ledger.CheckTxs (mempool.Pool.Batch, DR-12), so the block is one
// Append accepts while the tip is unchanged. The header links to the tip,
// commits to the Merkle root of the txids and carries the fixed difficulty;
// the nonce is left for the miner. Also returns the channel that is closed
// when this tip is replaced. The tip and the batch are read under one
// shared lock, so they belong together.
func (n *Node) assemble() (core.Block, <-chan struct{}, error) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.ledger == nil {
		return core.Block{}, nil, ErrStopped
	}
	_, tip, err := n.ledger.Tip()
	if err != nil {
		return core.Block{}, nil, err
	}
	txs, err := n.pool.Batch(n.p.MaxTxPerBlock)
	if err != nil {
		return core.Block{}, nil, err
	}
	hdr := core.Header{
		Version:    n.p.Genesis.Version,
		Prev:       core.HeaderHash(tip),
		MerkleRoot: core.MerkleRoot(core.TxIDs(txs)),
		Timestamp:  time.Now().Unix(),
		Difficulty: n.p.Difficulty,
	}
	return core.Block{Header: hdr, Txs: txs}, n.tipCh, nil
}

// mineOnce waits for pending transactions, assembles a block, mines it, and
// accepts and broadcasts it. Mining is cancelled when the tip changes
// (a peer's block or a chain switch), returning errTipChanged; the
// transactions stay pending (Batch does not remove them).
func (n *Node) mineOnce(ctx context.Context) (core.Block, error) {
	if err := n.pool.WaitNonEmpty(ctx); err != nil {
		return core.Block{}, err
	}
	blk, tipCh, err := n.assemble()
	if err != nil {
		return core.Block{}, err
	}
	if len(blk.Txs) == 0 { // nothing fits on this tip; never mine an empty block (M-R5)
		select {
		case <-tipCh:
		case <-ctx.Done():
			return core.Block{}, ctx.Err()
		case <-time.After(idleWait):
		}
		return core.Block{}, errTipChanged
	}

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-tipCh:
			cancel()
		case <-rctx.Done():
		}
	}()
	r, err := miner.Mine(rctx, blk.Header, n.p.Difficulty, n.opts.Workers)
	if err != nil {
		if ctx.Err() == nil && rctx.Err() != nil {
			return core.Block{}, errTipChanged
		}
		return core.Block{}, err
	}
	blk.Header.Nonce = r.Nonce

	if err := n.acceptOnTip(blk); err != nil {
		if errors.Is(err, errNotOnTip) {
			return core.Block{}, errTipChanged // a peer's block won the race
		}
		return core.Block{}, err
	}
	h, _ := n.Tip()
	n.logf("mined block %d %s with %d txs", h, r.Hash, len(blk.Txs))
	n.b.BroadcastBlock(blk)
	return blk, nil
}

// mineLoop mines until ctx is done or the mempool is closed.
func (n *Node) mineLoop(ctx context.Context) {
	for ctx.Err() == nil {
		_, err := n.mineOnce(ctx)
		switch {
		case err == nil, errors.Is(err, errTipChanged):
		case ctx.Err() != nil, errors.Is(err, mempool.ErrClosed), errors.Is(err, ErrStopped):
			return
		default:
			n.logf("mining: %v", err)
			select {
			case <-ctx.Done():
			case <-time.After(idleWait):
			}
		}
	}
}
