package node

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anon-HP/Aletheia/internal/consensus"
	"github.com/Anon-HP/Aletheia/internal/core"
	"github.com/Anon-HP/Aletheia/internal/mempool"
	"github.com/Anon-HP/Aletheia/internal/miner"
)

// Tests mine at a low difficulty (2^8 hashes per block on average) so that
// hundreds of blocks take well under a second; every rule is otherwise the
// demonstration network's.
const testDifficulty = 8

func testParams() consensus.Params {
	p := consensus.Default()
	p.Difficulty = testDifficulty
	return p
}

func acct(i int) core.Hash { return consensus.DemoAccount(i) }

func pay(from, to int, amount, fee, seq uint64) core.Tx {
	return core.Tx{Sender: acct(from), Recipient: acct(to), Amount: amount, Fee: fee, Seq: seq}
}

func newNode(t *testing.T, p consensus.Params, b Broadcaster) *Node {
	t.Helper()
	n, err := New(p, b, Options{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Stop)
	return n
}

func submit(t *testing.T, n *Node, tx core.Tx) core.Hash {
	t.Helper()
	id, err := n.SubmitTx(tx)
	if err != nil {
		t.Fatalf("submit %+v: %v", tx, err)
	}
	return id
}

// mine runs one assembly + mining round on n (the mempool must be non-empty).
func mine(t *testing.T, n *Node) core.Block {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	b, err := n.mineOnce(ctx)
	if err != nil {
		t.Fatalf("mineOnce: %v", err)
	}
	return b
}

// mineNext submits one small payment from sender (at its next seq on n) and
// mines it into a block.
func mineNext(t *testing.T, n *Node, sender int) core.Block {
	t.Helper()
	_, seq := n.Account(acct(sender))
	submit(t, n, pay(sender, 7, 1, 1, seq))
	return mine(t, n)
}

// buildBlock mines a block on prev with txs, outside any node.
func buildBlock(t *testing.T, p consensus.Params, prev core.Header, txs []core.Tx) core.Block {
	t.Helper()
	h := core.Header{Version: 1, Prev: core.HeaderHash(prev), MerkleRoot: core.MerkleRoot(core.TxIDs(txs)),
		Timestamp: time.Now().Unix(), Difficulty: p.Difficulty}
	r, err := miner.Mine(context.Background(), h, p.Difficulty, 2)
	if err != nil {
		t.Fatal(err)
	}
	h.Nonce = r.Nonce
	return core.Block{Header: h, Txs: txs}
}

func tipHash(n *Node) core.Hash {
	_, h := n.Tip()
	return core.HeaderHash(h)
}

func height(n *Node) uint64 {
	h, _ := n.Tip()
	return h
}

func fees(txs []core.Tx) []uint64 {
	var out []uint64
	for _, tx := range txs {
		out = append(out, tx.Fee)
	}
	return out
}

func eq(a, b []uint64) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

// currentLedger returns the node's ledger pointer (to observe a replay).
func currentLedger(n *Node) *core.Ledger {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.ledger
}

// ---- In-memory network ----

// network connects nodes through per-node endpoints (Broadcaster). Links are
// symmetric and start disconnected. With async delivery every broadcast
// block is delivered on its own goroutine, like a real network; otherwise
// delivery is synchronous, which makes fork scenarios deterministic.
type network struct {
	async    bool
	mu       sync.Mutex
	nodes    map[string]*Node
	links    map[[2]string]bool
	fetches  map[string]int // FetchBlocks calls per requester
	fetched  map[string]int // blocks transferred to each requester
	lastFork map[string]uint64
	inflight atomic.Int64
}

func newNetwork(async bool) *network {
	return &network{async: async, nodes: map[string]*Node{}, links: map[[2]string]bool{},
		fetches: map[string]int{}, fetched: map[string]int{}, lastFork: map[string]uint64{}}
}

type endpoint struct {
	net  *network
	name string
}

func (nw *network) add(t *testing.T, name string, p consensus.Params) *Node {
	n := newNode(t, p, endpoint{nw, name})
	nw.mu.Lock()
	nw.nodes[name] = n
	nw.mu.Unlock()
	return n
}

func (nw *network) link(a, b string, up bool) {
	nw.mu.Lock()
	defer nw.mu.Unlock()
	nw.links[[2]string{a, b}] = up
	nw.links[[2]string{b, a}] = up
}

func (nw *network) linkAll(up bool) {
	nw.mu.Lock()
	var names []string
	for k := range nw.nodes {
		names = append(names, k)
	}
	nw.mu.Unlock()
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			nw.link(names[i], names[j], up)
		}
	}
}

func (nw *network) peers(of string) map[string]*Node {
	nw.mu.Lock()
	defer nw.mu.Unlock()
	out := map[string]*Node{}
	for name, n := range nw.nodes {
		if name != of && nw.links[[2]string{of, name}] {
			out[name] = n
		}
	}
	return out
}

func (nw *network) stats(name string) (fetches, fetched int, fork uint64) {
	nw.mu.Lock()
	defer nw.mu.Unlock()
	return nw.fetches[name], nw.fetched[name], nw.lastFork[name]
}

func (e endpoint) BroadcastBlock(b core.Block) {
	for _, n := range e.net.peers(e.name) {
		if e.net.async {
			e.net.inflight.Add(1)
			go func(n *Node) {
				defer e.net.inflight.Add(-1)
				n.ReceiveBlock(b, e.name)
			}(n)
		} else {
			n.ReceiveBlock(b, e.name)
		}
	}
}

func (e endpoint) FetchBlocks(peer string, loc []consensus.LocatorEntry) (uint64, []core.Block, error) {
	n, ok := e.net.peers(e.name)[peer]
	if !ok {
		return 0, nil, fmt.Errorf("no link %s-%s", e.name, peer)
	}
	fork, bs, err := n.BlocksAfter(loc)
	e.net.mu.Lock()
	e.net.fetches[e.name]++
	e.net.fetched[e.name] += len(bs)
	e.net.lastFork[e.name] = fork
	e.net.mu.Unlock()
	return fork, bs, err
}

// scripted is a Broadcaster whose FetchBlocks returns a fixed answer.
type scripted struct {
	fork   uint64
	blocks []core.Block
	err    error
}

func (scripted) BroadcastBlock(core.Block) {}

func (s *scripted) FetchBlocks(string, []consensus.LocatorEntry) (uint64, []core.Block, error) {
	return s.fork, s.blocks, s.err
}

// ---- Single node ----

func TestNewArgs(t *testing.T) {
	p := testParams()
	p.MaxTxPerBlock = 0
	if _, err := New(p, nil, Options{}); !errors.Is(err, core.ErrArg) {
		t.Fatalf("MaxTxPerBlock 0: %v", err)
	}
	p = testParams()
	p.Difficulty = core.MaxDifficulty + 1
	if _, err := New(p, nil, Options{}); !errors.Is(err, core.ErrArg) {
		t.Fatalf("difficulty 257: %v", err)
	}
	p = testParams()
	p.Genesis.Prev[0] = 1
	if _, err := New(p, nil, Options{}); err == nil {
		t.Fatal("genesis with non-zero prev accepted")
	}
}

func TestGenesisState(t *testing.T) {
	p := testParams()
	n := newNode(t, p, nil)
	h, hd := n.Tip()
	if h != 0 || hd != p.Genesis {
		t.Fatalf("tip = %d %+v", h, hd)
	}
	if bal, seq := n.Account(acct(0)); bal != consensus.DemoBalance || seq != 0 {
		t.Fatalf("account = %d %d", bal, seq)
	}
	if hs := n.Headers(0); len(hs) != 1 || hs[0] != p.Genesis {
		t.Fatalf("headers = %v", hs)
	}
	if bs := n.Blocks(0); len(bs) != 1 || len(bs[0].Txs) != 0 {
		t.Fatalf("blocks = %v", bs)
	}
	loc := n.Locator()
	if len(loc) != 1 || loc[0] != (consensus.LocatorEntry{Height: 0, Hash: core.HeaderHash(p.Genesis)}) {
		t.Fatalf("locator = %v", loc)
	}
	if n.MempoolSize() != 0 || len(n.Mempool()) != 0 {
		t.Fatal("mempool not empty")
	}
	if err := n.Sync("x"); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("Sync without peers: %v", err)
	}
}

// Submitted transactions are mined highest fee first, MaxTxPerBlock at a
// time, into a block whose header links to the tip, commits to the txids
// and passes CheckHeader; confirmed ones leave the mempool.
func TestAssemblyFeeOrder(t *testing.T) {
	p := testParams()
	p.MaxTxPerBlock = 3
	n := newNode(t, p, nil)
	for i, fee := range []uint64{5, 50, 20, 1, 30} {
		submit(t, n, pay(i, 7, 100, fee, 0))
	}
	if n.MempoolSize() != 5 || !eq(fees(n.Mempool()), []uint64{50, 30, 20, 5, 1}) {
		t.Fatalf("mempool %v", fees(n.Mempool()))
	}

	b := mine(t, n)
	if !eq(fees(b.Txs), []uint64{50, 30, 20}) {
		t.Fatalf("block fees %v, want 50 30 20", fees(b.Txs))
	}
	hs := n.Headers(0)
	if len(hs) != 2 || hs[1] != b.Header {
		t.Fatalf("chain %v", hs)
	}
	if b.Header.Prev != core.HeaderHash(p.Genesis) || b.Header.MerkleRoot != core.MerkleRoot(core.TxIDs(b.Txs)) ||
		b.Header.Difficulty != p.Difficulty || b.Header.Version != p.Genesis.Version {
		t.Fatalf("header %+v", b.Header)
	}
	if err := consensus.CheckHeader(p, p.Genesis, b.Header); err != nil {
		t.Fatal(err)
	}
	if !eq(fees(n.Mempool()), []uint64{5, 1}) || n.MempoolSize() != 2 {
		t.Fatalf("mempool after block %v", fees(n.Mempool()))
	}
	if bal, seq := n.Account(acct(1)); bal != consensus.DemoBalance-150 || seq != 1 {
		t.Fatalf("payer account %d %d", bal, seq)
	}
	if bal, _ := n.Account(acct(7)); bal != consensus.DemoBalance+300 {
		t.Fatalf("payee balance %d", bal)
	}

	b2 := mine(t, n)
	if !eq(fees(b2.Txs), []uint64{5, 1}) || b2.Header.Prev != core.HeaderHash(b.Header) {
		t.Fatalf("second block %v", fees(b2.Txs))
	}
	if n.MempoolSize() != 0 || height(n) != 2 {
		t.Fatal("not drained")
	}
}

// DR-12 regression: transactions admitted one at a time can conflict
// inside a block. Two payments that each fit the recipient's balance but
// overflow it together must not both be assembled; the block appends and
// mining continues. Duplicate and same-sender submissions are rejected at
// admission and do not stall mining either.
func TestAssemblyUsesListCheck(t *testing.T) {
	p := testParams()
	p.Allocations[7].Balance = math.MaxUint64 - 10
	n := newNode(t, p, nil)

	a := pay(0, 7, 6, 3, 0) // highest fee
	b := pay(1, 7, 6, 2, 0) // conflicts with a: 7's balance would overflow
	c := pay(2, 3, 6, 1, 0) // independent
	for _, tx := range []core.Tx{a, b, c} {
		submit(t, n, tx)
	}
	if _, err := n.SubmitTx(a); !errors.Is(err, mempool.ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := n.SubmitTx(pay(0, 4, 1, 9, 0)); !errors.Is(err, mempool.ErrSenderPending) {
		t.Fatalf("same sender: %v", err)
	}
	// The conflict is real: the three together fail the ledger's list check.
	if idx, err := currentLedger(n).CheckTxs([]core.Tx{a, b, c}); idx != 1 || !errors.Is(err, core.ErrOverflow) {
		t.Fatalf("CheckTxs(a, b, c) = %d %v", idx, err)
	}

	blk := mine(t, n)
	if len(blk.Txs) != 2 || blk.Txs[0] != a || blk.Txs[1] != c {
		t.Fatalf("block %+v, want [a c]", blk.Txs)
	}
	if height(n) != 1 {
		t.Fatal("block not appended")
	}
	// b is now invalid against the chain (overflow) and was evicted.
	if n.MempoolSize() != 0 {
		t.Fatalf("mempool %+v", n.Mempool())
	}
	if _, err := n.SubmitTx(b); !errors.Is(err, core.ErrOverflow) {
		t.Fatalf("resubmitted b: %v", err)
	}
	// Mining continues normally.
	mineNext(t, n, 4)
	if height(n) != 2 {
		t.Fatal("mining stalled")
	}
}

// Milestone M4': a transaction submitted to a node is mined, and its proof
// verifies against the node's own header with core.MerkleVerify.
func TestProof(t *testing.T) {
	p := testParams()
	n := newNode(t, p, nil)
	mineNext(t, n, 7) // a first block, so the proofs are at height 2
	var txs []core.Tx
	for i := 0; i < 7; i++ {
		tx := pay(i, (i+1)%8, uint64(10+i), uint64(1+i), 0)
		submit(t, n, tx)
		txs = append(txs, tx)
	}
	mine(t, n)
	hdr := n.Headers(2)[0]
	for _, tx := range txs {
		r, err := n.Proof(core.TxID(tx))
		if err != nil {
			t.Fatal(err)
		}
		if r.Height != 2 || r.Tx != tx || len(r.Proof) != 3 { // ceil(log2 7)
			t.Fatalf("proof %+v", r)
		}
		if !core.MerkleVerify(hdr.MerkleRoot, core.TxID(r.Tx), r.Proof) {
			t.Fatal("proof does not verify")
		}
		bad := append(core.Proof(nil), r.Proof...)
		bad[0].Sibling[0] ^= 1
		if core.MerkleVerify(hdr.MerkleRoot, core.TxID(r.Tx), bad) {
			t.Fatal("tampered proof verifies")
		}
		if core.MerkleVerify(n.Headers(1)[0].MerkleRoot, core.TxID(r.Tx), r.Proof) {
			t.Fatal("proof verifies against another block's root")
		}
	}
	if _, err := n.Proof(core.TxID(pay(0, 1, 1, 1, 99))); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown txid: %v", err)
	}
}

// The mining loop mines only when transactions are pending (no empty
// blocks), confirms a stream of transactions including a conflicting pair,
// and Stop ends it and frees the node.
func TestMiningLoopAndStop(t *testing.T) {
	p := testParams()
	p.Allocations[7].Balance = math.MaxUint64 - 10
	var logs atomic.Int64
	n, err := New(p, nil, Options{Workers: 2, Logf: func(string, ...any) { logs.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Stop()
	n.Start(context.Background())
	n.Start(context.Background()) // no effect

	time.Sleep(200 * time.Millisecond)
	if height(n) != 0 {
		t.Fatal("mined with an empty mempool")
	}

	submit(t, n, pay(1, 7, 6, 2, 0))
	submit(t, n, pay(2, 7, 6, 3, 0)) // conflicts with the first
	const k = 10
	for seq := uint64(0); seq < k; seq++ {
		submit(t, n, pay(0, 3, 1, 1, seq))
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, next := n.Account(acct(0)); next > seq {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("seq %d not mined", seq)
			}
			time.Sleep(time.Millisecond)
		}
	}
	for n.MempoolSize() != 0 {
		time.Sleep(time.Millisecond)
	}
	_, s1 := n.Account(acct(1))
	_, s2 := n.Account(acct(2))
	if s1+s2 != 1 {
		t.Fatalf("conflicting pair: seqs %d %d, want exactly one confirmed", s1, s2)
	}
	hs := n.Headers(0)
	if err := consensus.CheckHeaderChain(p, hs); err != nil {
		t.Fatal(err)
	}
	for _, b := range n.Blocks(1) {
		if len(b.Txs) == 0 {
			t.Fatal("empty block")
		}
	}
	if logs.Load() == 0 {
		t.Fatal("no log lines")
	}

	n.Stop()
	n.Stop()
	n.Start(context.Background()) // no effect after Stop
	if h, hd := n.Tip(); h != 0 || hd != (core.Header{}) {
		t.Fatal("Tip after Stop")
	}
	if n.Headers(0) != nil || n.Blocks(0) != nil || n.Locator() != nil || n.Mempool() != nil || n.MempoolSize() != 0 {
		t.Fatal("reads after Stop")
	}
	if bal, seq := n.Account(acct(0)); bal != 0 || seq != 0 {
		t.Fatal("Account after Stop")
	}
	if _, err := n.SubmitTx(pay(5, 6, 1, 1, 0)); !errors.Is(err, mempool.ErrClosed) {
		t.Fatalf("SubmitTx after Stop: %v", err)
	}
	if _, err := n.Proof(core.Hash{}); !errors.Is(err, ErrStopped) {
		t.Fatalf("Proof after Stop: %v", err)
	}
	if _, _, err := n.HeadersAfter(nil); !errors.Is(err, ErrStopped) {
		t.Fatalf("HeadersAfter after Stop: %v", err)
	}
	if err := n.Sync("x"); !errors.Is(err, ErrStopped) {
		t.Fatalf("Sync after Stop: %v", err)
	}
	if err := n.ReceiveBlock(core.Block{}, "x"); !errors.Is(err, ErrStopped) {
		t.Fatalf("ReceiveBlock after Stop: %v", err)
	}
}

// A tip change cancels the block being mined (DR-7); its transactions stay
// pending.
func TestMiningCancelledOnNewTip(t *testing.T) {
	p := testParams()
	p.Difficulty = 40 // never solved within the test
	n := newNode(t, p, nil)
	submit(t, n, pay(0, 1, 1, 1, 0))
	errc := make(chan error, 1)
	go func() {
		_, err := n.mineOnce(context.Background())
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	n.mu.Lock()
	n.tipChanged()
	n.mu.Unlock()
	select {
	case err := <-errc:
		if !errors.Is(err, errTipChanged) {
			t.Fatalf("mineOnce: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mining not cancelled")
	}
	t.Logf("cancelled after %v", time.Since(start))
	if n.MempoolSize() != 1 {
		t.Fatal("transaction lost")
	}
}

// ReceiveBlock on the tip: accepted, mempool reconciled, miner signalled;
// every invalid variant is rejected with its reason and changes nothing;
// a block already held is stale.
func TestReceiveBlockOnTip(t *testing.T) {
	p := testParams()
	a := newNode(t, p, nil)
	b := newNode(t, p, nil)
	tx := pay(0, 1, 5, 1, 0)
	submit(t, a, tx)
	submit(t, b, tx)
	blk := mine(t, a)

	b.mu.RLock()
	tipCh := b.tipCh
	b.mu.RUnlock()
	if err := b.ReceiveBlock(blk, "a"); err != nil {
		t.Fatal(err)
	}
	if tipHash(b) != tipHash(a) || b.MempoolSize() != 0 {
		t.Fatal("block not applied or mempool not reconciled")
	}
	select {
	case <-tipCh:
	default:
		t.Fatal("miner not signalled")
	}
	if err := b.ReceiveBlock(blk, "a"); !errors.Is(err, ErrStale) {
		t.Fatalf("duplicate: %v", err)
	}

	// Invalid blocks, offered to a node at genesis.
	c := newNode(t, p, nil)
	good := buildBlock(t, p, p.Genesis, []core.Tx{pay(2, 3, 5, 1, 0)})
	badWork := good
	for core.PowCheck(core.HeaderHash(badWork.Header), p.Difficulty) {
		badWork.Header.Nonce++
	}
	badDiff := good
	badDiff.Header.Difficulty = p.Difficulty + 1
	badMerkle := good
	badMerkle.Txs = []core.Tx{pay(2, 3, 6, 1, 0)} // header still commits to the original
	cases := map[string]struct {
		blk  core.Block
		want error
	}{
		"work":       {badWork, consensus.ErrWork},
		"difficulty": {badDiff, consensus.ErrDifficulty},
		"merkle":     {badMerkle, core.ErrMerkle},
		"balance":    {buildBlock(t, p, p.Genesis, []core.Tx{pay(2, 3, consensus.DemoBalance, 1, 0)}), core.ErrBalance},
		"seq":        {buildBlock(t, p, p.Genesis, []core.Tx{pay(2, 3, 1, 1, 1)}), core.ErrSeq},
	}
	for name, tc := range cases {
		if err := c.ReceiveBlock(tc.blk, "x"); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v, want %v", name, err, tc.want)
		}
		if height(c) != 0 {
			t.Fatalf("%s: chain changed", name)
		}
	}
	if err := c.ReceiveBlock(good, "x"); err != nil {
		t.Fatal(err)
	}
}

// Headers, Blocks, Locator, HeadersAfter, BlocksAfter on a 25-block chain.
func TestChainReads(t *testing.T) {
	p := testParams()
	n := newNode(t, p, nil)
	for i := 0; i < 25; i++ {
		mineNext(t, n, i%7)
	}
	hs := n.Headers(0)
	if len(hs) != 26 {
		t.Fatalf("%d headers", len(hs))
	}
	if err := consensus.CheckHeaderChain(p, hs); err != nil {
		t.Fatal(err)
	}
	at := func(h uint64) core.Hash { return core.HeaderHash(hs[h]) }
	bs := n.Blocks(20)
	if len(bs) != 6 || bs[0].Header != hs[20] || len(bs[0].Txs) != 1 {
		t.Fatal("Blocks(20)")
	}
	if n.Headers(26) != nil || n.Blocks(26) != nil {
		t.Fatal("reads above the tip")
	}
	if fmt.Sprint(n.Locator()) != fmt.Sprint(consensus.BuildLocator(25, at)) {
		t.Fatal("Locator")
	}

	fork, after, err := n.HeadersAfter(n.Locator())
	if err != nil || fork != 25 || len(after) != 0 {
		t.Fatalf("own locator: %d %d %v", fork, len(after), err)
	}
	fork, after, err = n.HeadersAfter(consensus.BuildLocator(10, at))
	if err != nil || fork != 10 || len(after) != 15 || after[0] != hs[11] || after[14] != hs[25] {
		t.Fatalf("prefix locator: %d %d %v", fork, len(after), err)
	}
	fork, blocks, err := n.BlocksAfter(consensus.BuildLocator(10, at))
	if err != nil || fork != 10 || len(blocks) != 15 || blocks[0].Header != hs[11] {
		t.Fatalf("BlocksAfter: %d %d %v", fork, len(blocks), err)
	}
	// A requester on a branch from height 12, and one with a longer chain.
	other := make([]core.Hash, 40)
	for h := range other {
		if h <= 12 {
			other[h] = at(uint64(h))
		} else {
			other[h] = core.SHA256([]byte{byte(h)})
		}
	}
	fork, after, err = n.HeadersAfter(consensus.BuildLocator(39, func(h uint64) core.Hash { return other[h] }))
	if err != nil || fork > 12 || len(after) != int(25-fork) {
		t.Fatalf("branch locator: %d %v", fork, err)
	}

	for _, bad := range [][]consensus.LocatorEntry{nil, {{Height: 1}, {Height: 2}}, {{Height: 3, Hash: at(3)}}} {
		if _, _, err := n.HeadersAfter(bad); !errors.Is(err, consensus.ErrBadLocator) {
			t.Fatalf("%v: %v", bad, err)
		}
		if _, _, err := n.BlocksAfter(bad); !errors.Is(err, consensus.ErrBadLocator) {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	if _, _, err := n.BlocksAfter([]consensus.LocatorEntry{{Height: 0}}); !errors.Is(err, consensus.ErrGenesisMismatch) {
		t.Fatalf("genesis mismatch: %v", err)
	}
}

// ---- Multi-node ----

// Two nodes mine on the same tip while partitioned. On equal length each
// keeps its own chain (first seen); when one branch gets longer, the other
// node switches by replaying from genesis, and its abandoned transactions
// return to its mempool if the new chain has not confirmed them.
func TestForkResolution(t *testing.T) {
	p := testParams()
	nw := newNetwork(false)
	a := nw.add(t, "a", p)
	b := nw.add(t, "b", p)
	nw.link("a", "b", true)
	mineNext(t, a, 6)
	mineNext(t, a, 6) // relayed to b
	if height(b) != 2 || tipHash(b) != tipHash(a) {
		t.Fatal("b did not follow a")
	}
	nw.link("a", "b", false)

	onlyA := pay(0, 4, 10, 2, 0)
	shared := pay(2, 4, 10, 1, 0) // submitted to both
	onlyB := pay(1, 4, 10, 2, 0)
	submit(t, a, onlyA)
	submit(t, a, shared)
	mine(t, a)
	submit(t, b, onlyB)
	submit(t, b, shared)
	mine(t, b)
	if height(a) != 3 || height(b) != 3 || tipHash(a) == tipHash(b) {
		t.Fatal("no fork")
	}

	nw.link("a", "b", true)
	if err := a.Sync("b"); !errors.Is(err, ErrStale) {
		t.Fatalf("equal length: %v", err)
	}
	if err := b.Sync("a"); !errors.Is(err, ErrStale) {
		t.Fatalf("equal length: %v", err)
	}

	oldA := currentLedger(a)
	a.mu.RLock()
	tipCh := a.tipCh
	a.mu.RUnlock()
	before, beforeBlocks, _ := nw.stats("a")
	mineNext(t, b, 3) // b's branch is now longer; relayed to a, which syncs
	if height(a) != 4 || tipHash(a) != tipHash(b) {
		t.Fatalf("a did not switch: %d", height(a))
	}
	select {
	case <-tipCh: // a's miner would abandon its attempt on the old tip
	default:
		t.Fatal("miner not signalled on the chain switch")
	}
	if currentLedger(a) == oldA {
		t.Fatal("no fresh ledger")
	}
	if _, err := oldA.Height(); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("old ledger not closed: %v", err)
	}
	after, afterBlocks, fork := nw.stats("a")
	if after-before != 1 || fork != 2 || afterBlocks-beforeBlocks != 2 {
		t.Fatalf("a fetched %d blocks in %d requests, fork %d", afterBlocks-beforeBlocks, after-before, fork)
	}
	// onlyA was abandoned and is still valid; shared is confirmed on b's chain.
	if pend := a.Mempool(); len(pend) != 1 || pend[0] != onlyA {
		t.Fatalf("a's mempool %+v", pend)
	}
	if _, err := a.Proof(core.TxID(shared)); err != nil {
		t.Fatal("shared tx not confirmed")
	}
	if _, err := a.Proof(core.TxID(onlyA)); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("abandoned tx still confirmed")
	}

	mine(t, a) // the returned transaction is mined on the new chain
	if height(b) != 5 || tipHash(b) != tipHash(a) {
		t.Fatal("b did not follow")
	}
	for _, n := range []*Node{a, b} {
		for _, tx := range []core.Tx{onlyA, onlyB, shared} {
			if _, err := n.Proof(core.TxID(tx)); err != nil {
				t.Fatalf("tx %+v: %v", tx, err)
			}
		}
	}
}

// Forks at depths 1, 5 and 30 below a shared 40-block prefix: the node on
// the shorter branch syncs with one locator request that transfers only the
// blocks above the locator's fork point, then replays from genesis.
func TestForkDepths(t *testing.T) {
	const prefix = 40
	for _, k := range []uint64{1, 5, 30} {
		t.Run(fmt.Sprintf("depth%d", k), func(t *testing.T) {
			p := testParams()
			nw := newNetwork(false)
			a := nw.add(t, "a", p)
			b := nw.add(t, "b", p)
			for i := 0; i < prefix; i++ {
				mineNext(t, a, 5)
			}
			nw.link("a", "b", true)
			if err := b.Sync("a"); err != nil || tipHash(b) != tipHash(a) {
				t.Fatalf("prefix sync: %v", err)
			}
			nw.link("a", "b", false)

			var abandoned []core.Tx
			for i := uint64(0); i < k; i++ {
				abandoned = append(abandoned, mineNext(t, a, 0).Txs...)
			}
			for i := uint64(0); i <= k; i++ {
				mineNext(t, b, 1)
			}
			nw.link("a", "b", true)
			before, beforeBlocks, _ := nw.stats("a")
			oldA := currentLedger(a)
			_, tipB := b.Tip()
			tipBlock := b.Blocks(prefix + k + 1)[0]
			if tipBlock.Header != tipB {
				t.Fatal("tip block")
			}
			if err := a.ReceiveBlock(tipBlock, "b"); err != nil {
				t.Fatal(err)
			}

			if height(a) != prefix+k+1 || tipHash(a) != tipHash(b) {
				t.Fatal("not converged")
			}
			fetches, fetched, fork := nw.stats("a")
			fetched -= beforeBlocks
			// The responder returns the highest entry of a's locator at or
			// below the true common ancestor (height prefix).
			var want uint64
			for _, h := range consensus.LocatorHeights(prefix + k) {
				if h <= prefix {
					want = h
					break
				}
			}
			if fetches-before != 1 || fork != want || uint64(fetched) != prefix+k+1-want {
				t.Fatalf("fetch: %d requests, fork %d (want %d), %d blocks", fetches-before, fork, want, fetched)
			}
			if prefix+k-fork > 2*k+10 {
				t.Fatalf("over-fetch beyond bound: fork %d", fork)
			}
			t.Logf("depth %d: fork point %d, %d of %d blocks transferred", k, fork, fetched, prefix+k+1)
			if currentLedger(a) == oldA {
				t.Fatal("no replay into a fresh ledger")
			}
			for i := 0; i < consensus.DemoAccounts; i++ {
				ba, sa := a.Account(acct(i))
				bb, sb := b.Account(acct(i))
				if ba != bb || sa != sb {
					t.Fatalf("account %d differs after replay", i)
				}
			}
			// One pending transaction per sender: the first abandoned one
			// returns; its successors must be resent by the client (O-4).
			if pend := a.Mempool(); len(pend) != 1 || pend[0] != abandoned[0] {
				t.Fatalf("a's mempool %+v", pend)
			}
		})
	}
}

// A node that joins late catches up with one locator request, either by an
// explicit Sync or when the next relayed block reveals the gap.
func TestLateJoiningNode(t *testing.T) {
	p := testParams()
	nw := newNetwork(false)
	a := nw.add(t, "a", p)
	b := nw.add(t, "b", p)
	nw.link("a", "b", true)
	for i := 0; i < 35; i++ {
		mineNext(t, a, i%4)
	}
	if tipHash(b) != tipHash(a) {
		t.Fatal("b did not follow")
	}

	c := nw.add(t, "c", p)
	nw.link("c", "a", true)
	if err := c.Sync("a"); err != nil {
		t.Fatal(err)
	}
	if fetches, fetched, fork := nw.stats("c"); fetches != 1 || fetched != 35 || fork != 0 || tipHash(c) != tipHash(a) {
		t.Fatalf("c: %d requests, %d blocks, fork %d", fetches, fetched, fork)
	}
	if err := c.Sync("a"); !errors.Is(err, ErrStale) {
		t.Fatalf("second sync: %v", err)
	}

	d := nw.add(t, "d", p)
	nw.link("d", "b", true)
	mineNext(t, a, 6) // a → b → (relay) d, which is 36 behind and syncs with b
	if height(d) != 36 || tipHash(d) != tipHash(a) || tipHash(c) != tipHash(a) {
		t.Fatalf("d at %d", height(d))
	}
	if fetches, fetched, _ := nw.stats("d"); fetches != 1 || fetched != 36 {
		t.Fatalf("d: %d requests, %d blocks", fetches, fetched)
	}
}

// An invalid peer chain is rejected and the node keeps its ledger, its tip
// and its mempool.
func TestInvalidPeerChainRejected(t *testing.T) {
	p := testParams()
	src := newNode(t, p, nil)
	for i := 0; i < 5; i++ {
		mineNext(t, src, i)
	}
	peer := src.Blocks(1) // a valid 5-block chain from genesis

	sc := &scripted{}
	n := newNode(t, p, sc)
	own := mineNext(t, n, 6).Txs[0]
	mineNext(t, n, 6)
	pending := pay(5, 4, 1, 1, 0)
	submit(t, n, pending)
	oldLedger, oldTip := currentLedger(n), tipHash(n)

	clone := func() []core.Block {
		out := make([]core.Block, len(peer))
		for i, b := range peer {
			out[i] = core.Block{Header: b.Header, Txs: append([]core.Tx(nil), b.Txs...)}
		}
		return out
	}
	badWork := clone()
	for core.PowCheck(core.HeaderHash(badWork[2].Header), p.Difficulty) {
		badWork[2].Header.Nonce++
	}
	badTx := clone()
	badTx[1].Txs[0].Amount++
	badDiff := clone()
	badDiff[3].Header.Difficulty++
	badLink := clone()[1:]
	twoBlocks := clone()[:2]

	cases := []struct {
		name   string
		fork   uint64
		blocks []core.Block
		err    error
		want   []error
	}{
		{"proof of work", 0, badWork, nil, []error{ErrPeerChain, consensus.ErrWork}},
		{"merkle", 0, badTx, nil, []error{ErrPeerChain, core.ErrMerkle}},
		{"difficulty", 0, badDiff, nil, []error{ErrPeerChain, consensus.ErrDifficulty}},
		{"first block does not link to the fork point", 0, badLink, nil, []error{ErrPeerChain}},
		{"fork point above own tip", 3, clone(), nil, []error{ErrPeerChain}},
		{"not longer", 0, twoBlocks, nil, []error{ErrStale}},
		{"nothing after the fork", 2, nil, nil, []error{ErrStale}},
		{"fetch error", 0, nil, errors.New("timeout"), nil},
	}
	for _, tc := range cases {
		sc.fork, sc.blocks, sc.err = tc.fork, tc.blocks, tc.err
		err := n.Sync("peer")
		if err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
		for _, w := range tc.want {
			if !errors.Is(err, w) {
				t.Fatalf("%s: %v, want %v", tc.name, err, w)
			}
		}
		if currentLedger(n) != oldLedger || tipHash(n) != oldTip || height(n) != 2 {
			t.Fatalf("%s: chain changed", tc.name)
		}
		if pend := n.Mempool(); len(pend) != 1 || pend[0] != pending {
			t.Fatalf("%s: mempool changed", tc.name)
		}
	}
	if _, err := oldLedger.Height(); err != nil {
		t.Fatalf("old ledger unusable: %v", err)
	}

	sc.fork, sc.blocks, sc.err = 0, clone(), nil
	if err := n.Sync("peer"); err != nil {
		t.Fatal(err)
	}
	if tipHash(n) != tipHash(src) {
		t.Fatal("valid longer chain not adopted")
	}
	// The pending transaction is still valid on the new chain, and the first
	// of the node's own abandoned transactions is re-admitted (its successor
	// is not: one pending transaction per sender).
	pend := n.Mempool()
	if len(pend) != 2 || !((pend[0] == pending && pend[1] == own) || (pend[0] == own && pend[1] == pending)) {
		t.Fatalf("mempool after switch %+v", pend)
	}
}

// clientTx is sender s's transaction with sequence number seq in
// TestThreeNodesConverge (deterministic, so a resend is the same transaction).
func clientTx(s int, seq uint64) core.Tx { return pay(s, 7, 1+seq, 1+uint64(s), seq) }

// Three nodes mining concurrently with asynchronous block delivery converge
// to one chain that confirms every submitted transaction exactly once,
// while readers hammer every read method (run with -race).
func TestThreeNodesConverge(t *testing.T) {
	p := testParams()
	nw := newNetwork(true)
	names := []string{"n0", "n1", "n2"}
	var nodes []*Node
	var switches, mined atomic.Int64
	logf := func(format string, args ...any) {
		switch {
		case strings.HasPrefix(format, "switched"):
			switches.Add(1)
		case strings.HasPrefix(format, "mined"):
			mined.Add(1)
		}
	}
	for _, name := range names {
		n, err := New(p, endpoint{nw, name}, Options{Workers: 2, Logf: logf})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(n.Stop)
		nw.mu.Lock()
		nw.nodes[name] = n
		nw.mu.Unlock()
		nodes = append(nodes, n)
	}
	nw.linkAll(true)
	t.Cleanup(func() {
		for nw.inflight.Load() != 0 {
			time.Sleep(time.Millisecond)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, n := range nodes {
		n.Start(ctx)
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i, n := range nodes {
		readers.Add(1)
		go func(i int, n *Node) {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				h, _ := n.Tip()
				n.Headers(h)
				n.Blocks(h)
				n.HeadersAfter(nodes[(i+1)%3].Locator())
				n.Proof(core.TxID(pay(0, 7, 1, 1, 0)))
				n.Account(acct(i))
				n.Mempool()
				n.MempoolSize()
				time.Sleep(time.Millisecond)
			}
		}(i, n)
	}

	// Equal-length forks persist while no node mines (first seen wins, and
	// blocks are never empty); the next block resolves them. Real traffic
	// provides that block; here a tie-breaker adds one filler transaction
	// whenever the network is idle, every mempool is empty and tips differ.
	settled := func() (same, quiet bool) {
		same = tipHash(nodes[0]) == tipHash(nodes[1]) && tipHash(nodes[1]) == tipHash(nodes[2])
		quiet = nw.inflight.Load() == 0
		for _, n := range nodes {
			quiet = quiet && n.MempoolSize() == 0
		}
		return same, quiet
	}
	var fillers atomic.Int64
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			if same, quiet := settled(); quiet && !same {
				_, seq := nodes[0].Account(acct(7))
				if _, err := nodes[0].SubmitTx(pay(7, 6, 1, 1, seq)); err == nil {
					fillers.Add(1)
				}
			}
		}
	}()

	// Six senders, eight transactions each, each sent to a different node
	// in turn. A client waits until its transaction is confirmed on the node
	// it used, resending while it is not. Resending is part of the protocol:
	// after a chain switch only the first of a sender's abandoned
	// transactions returns to the mempool (one pending transaction per
	// sender, O-4), and a node on the other side of a fork rejects a
	// successor until it has the predecessor.
	const senders, perSender = 6, 8
	deadline := time.Now().Add(60 * time.Second)
	var clients sync.WaitGroup
	errc := make(chan error, senders)
	for s := 0; s < senders; s++ {
		clients.Add(1)
		go func(s int) {
			defer clients.Done()
			for seq := uint64(0); seq < perSender; seq++ {
				n := nodes[(s+int(seq))%3]
				for {
					_, next := n.Account(acct(s))
					if next > seq {
						break
					}
					// Send what this node needs next: seq itself, or an
					// earlier transaction its chain lacks (lost by a reorg
					// under O-4, or not yet relayed). Rejections while it
					// is pending (ErrDuplicate, ErrSenderPending) are fine.
					n.SubmitTx(clientTx(s, next))
					if time.Now().After(deadline) {
						errc <- fmt.Errorf("sender %d seq %d not confirmed", s, seq)
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		}(s)
	}
	clients.Wait()
	close(errc)
	diag := func() {
		for i, n := range nodes {
			var seqs []uint64
			for s := 0; s < senders; s++ {
				_, next := n.Account(acct(s))
				seqs = append(seqs, next)
			}
			t.Logf("node %d: height %d tip %.8s mempool %d inflight %d next seqs %v",
				i, height(n), tipHash(n), n.MempoolSize(), nw.inflight.Load(), seqs)
		}
	}
	for err := range errc {
		diag()
		t.Fatal(err)
	}
	for {
		if same, quiet := settled(); same && quiet {
			break
		}
		if time.Now().After(deadline) {
			diag()
			t.Fatal("no convergence")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	readers.Wait()

	hs := nodes[0].Headers(0)
	if err := consensus.CheckHeaderChain(p, hs); err != nil {
		t.Fatal(err)
	}
	seen := map[core.Hash]bool{}
	for _, b := range nodes[0].Blocks(1) {
		for _, tx := range b.Txs {
			id := core.TxID(tx)
			if seen[id] {
				t.Fatalf("tx confirmed twice: %+v", tx)
			}
			seen[id] = true
		}
	}
	for _, n := range nodes {
		for s := 0; s < senders; s++ {
			if _, next := n.Account(acct(s)); next != perSender {
				t.Fatalf("sender %d: next seq %d", s, next)
			}
			for seq := uint64(0); seq < perSender; seq++ {
				if _, err := n.Proof(core.TxID(clientTx(s, seq))); err != nil {
					t.Fatalf("sender %d seq %d: %v", s, seq, err)
				}
			}
		}
	}
	t.Logf("converged at height %d; %d blocks mined in total, %d chain switches, %d tie-breakers",
		len(hs)-1, mined.Load(), switches.Load(), fillers.Load())
}
