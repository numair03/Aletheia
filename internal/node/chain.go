package node

import (
	"errors"
	"fmt"

	"github.com/Anon-HP/Aletheia/internal/consensus"
	"github.com/Anon-HP/Aletheia/internal/core"
)

// Chain selection (DECISIONS DR-7, DR-14; CD-27): the longest valid chain by
// block count wins (the difficulty is fixed, so count = work); on a tie the
// chain already held stays. A longer chain is adopted by replaying it from
// genesis into a fresh ledger, and the node switches to that ledger only if
// every block of it is valid. There is no undo log and no partial switch.

var (
	// ErrStale: the block or chain offered is not longer than the node's own
	// (a duplicate, an old block, or an equal or shorter fork). Nothing
	// changed; this is the normal outcome for relayed blocks already held.
	ErrStale = errors.New("node: block does not lead to a longer chain")
	// ErrPeerChain: a peer's chain failed validation (bad fork point,
	// linkage, proof of work, or a ledger rule). The node keeps its chain.
	// The underlying consensus or ledger error is wrapped too.
	ErrPeerChain = errors.New("node: invalid peer chain")
)

// staleWindow is how far below the tip ReceiveBlock looks for a block's
// parent before asking the peer for its chain. A parent found there means
// the block is at most as high as the tip: a duplicate or a losing fork,
// not worth a round trip.
const staleWindow = 16

// ReceiveBlock handles a block sent by fromPeer (DR-7 step 2):
//   - if it builds on the tip: CheckHeader, Append, reconcile the mempool,
//     cancel the miner's current attempt, and relay it to the peers;
//   - if its parent is one of the last staleWindow blocks below the tip, it
//     cannot lead to a longer chain: ErrStale;
//   - otherwise the node is missing blocks or is on another branch: it syncs
//     with fromPeer (Sync), which adopts the peer's chain only if it is
//     longer and valid.
func (n *Node) ReceiveBlock(b core.Block, fromPeer string) error {
	err := n.acceptOnTip(b)
	if err == nil {
		h, _ := n.Tip()
		n.logf("accepted block %d from %s", h, fromPeer)
		n.b.BroadcastBlock(b)
		return nil
	}
	if !errors.Is(err, errNotOnTip) {
		return err
	}
	if n.parentNearTip(b.Header.Prev) {
		return ErrStale
	}
	return n.Sync(fromPeer)
}

// parentNearTip reports whether prev is the hash of one of the blocks
// tip-1 .. tip-staleWindow (the tip itself was already ruled out).
func (n *Node) parentNearTip(prev core.Hash) bool {
	found := false
	n.view(func(l *core.Ledger) error {
		tip, err := l.Height()
		if err != nil {
			return err
		}
		for i := uint64(1); i <= staleWindow && i <= tip; i++ {
			hd, err := l.HeaderAt(tip - i)
			if err != nil {
				return err
			}
			if core.HeaderHash(hd) == prev {
				found = true
				return nil
			}
		}
		return nil
	})
	return found
}

// Sync asks peer for the blocks after the common ancestor of the two chains
// (one locator request, DR-14) and adopts the peer's chain if it is longer
// and valid. Returns nil if the node switched, ErrStale if the peer's chain
// is not longer, an error wrapping ErrPeerChain if it is invalid, or the
// fetch error. A node that starts late calls Sync for its peers to catch up.
func (n *Node) Sync(peer string) error {
	loc := n.Locator()
	if loc == nil {
		return ErrStopped
	}
	fork, blocks, err := n.b.FetchBlocks(peer, loc)
	if err != nil {
		return fmt.Errorf("node: fetch from %s: %w", peer, err)
	}
	return n.adopt(peer, fork, blocks)
}

// adopt replaces the chain with own blocks 1..fork followed by blocks, if
// that chain is longer and valid. The fetch ran without locks, so the
// node's chain may have changed since its locator was built; every check
// here is made against the chain as it is now, under chainMu.
func (n *Node) adopt(peer string, fork uint64, blocks []core.Block) error {
	n.chainMu.Lock()
	defer n.chainMu.Unlock()
	// chainMu alone keeps n.ledger fixed: it changes only under chainMu.
	old := n.ledger
	if old == nil {
		return ErrStopped
	}
	height, err := old.Height()
	if err != nil {
		return err
	}
	if len(blocks) == 0 {
		return ErrStale
	}
	if fork > height {
		return fmt.Errorf("%w: fork point %d above own tip %d", ErrPeerChain, fork, height)
	}
	candidate := fork + uint64(len(blocks))
	if !consensus.Prefer(candidate, height) {
		return ErrStale
	}
	forkHdr, err := old.HeaderAt(fork)
	if err != nil {
		return err
	}
	if blocks[0].Header.Prev != core.HeaderHash(forkHdr) {
		return fmt.Errorf("%w: first block does not link to own block %d", ErrPeerChain, fork)
	}

	fresh, err := n.replay(old, fork, blocks)
	if err != nil {
		return err
	}

	// Transactions of the own blocks being abandoned, in chain order, to be
	// offered to the mempool again. Blocks the peer also has (over-fetch
	// above the true fork) are included too; their transactions are
	// confirmed on the new chain and admission drops them.
	var abandoned []core.Tx
	for h := fork + 1; h <= height; h++ {
		txs, err := old.BlockTxs(h)
		if err != nil {
			fresh.Close()
			return err
		}
		abandoned = append(abandoned, txs...)
	}

	n.mu.Lock()
	n.ledger = fresh
	evicted, readmitted, rerr := n.pool.Reconcile(fresh, abandoned)
	n.tipChanged() // cancels the miner's attempt on the old tip
	n.mu.Unlock()
	old.Close() // no reader holds it: readers take the pointer under mu, and the pool switched away
	if rerr != nil {
		n.logf("mempool reconcile after switch: %v", rerr)
	}
	n.logf("switched to %s's chain: height %d -> %d, fork %d, %d txs evicted, %d re-admitted",
		peer, height, candidate, fork, evicted, readmitted)
	n.b.BroadcastBlock(blocks[len(blocks)-1])
	return nil
}

// replay builds a fresh ledger from genesis: own blocks 1..fork (read from
// old), then blocks, each checked with consensus.CheckHeader and then
// appended (which runs every ledger rule and rebuilds the world state). On
// any failure the fresh ledger is freed and the error wraps ErrPeerChain;
// old is never modified. Caller holds chainMu.
func (n *Node) replay(old *core.Ledger, fork uint64, blocks []core.Block) (*core.Ledger, error) {
	fresh, err := core.NewLedger(n.p.Genesis, n.p.Allocations)
	if err != nil {
		return nil, err
	}
	prev := n.p.Genesis
	appendChecked := func(b core.Block) error {
		if err := consensus.CheckHeader(n.p, prev, b.Header); err != nil {
			return err
		}
		if err := fresh.Append(b); err != nil {
			return err
		}
		prev = b.Header
		return nil
	}
	for h := uint64(1); h <= fork; h++ {
		hd, err := old.HeaderAt(h)
		if err == nil {
			var txs []core.Tx
			if txs, err = old.BlockTxs(h); err == nil {
				err = appendChecked(core.Block{Header: hd, Txs: txs})
			}
		}
		if err != nil { // own blocks were valid when accepted: an internal failure
			fresh.Close()
			return nil, fmt.Errorf("node: replay of own block %d: %w", h, err)
		}
	}
	for i, b := range blocks {
		if err := appendChecked(b); err != nil {
			fresh.Close()
			return nil, fmt.Errorf("%w: block %d: %w", ErrPeerChain, fork+1+uint64(i), err)
		}
	}
	return fresh, nil
}
