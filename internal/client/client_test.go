package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Anon-HP/Aletheia/internal/api"
	"github.com/Anon-HP/Aletheia/internal/consensus"
	"github.com/Anon-HP/Aletheia/internal/core"
	"github.com/Anon-HP/Aletheia/internal/mempool"
	"github.com/Anon-HP/Aletheia/internal/miner"
	"github.com/Anon-HP/Aletheia/internal/node"
	"github.com/Anon-HP/Aletheia/internal/routing"
)

// Tests mine at difficulty 8 (2^8 hashes per block on average); every other
// rule is the demonstration network's.
const testDifficulty = 8

const clientID = "light-1"

var ctx = context.Background()

func testParams() consensus.Params {
	p := consensus.Default()
	p.Difficulty = testDifficulty
	return p
}

func acct(i int) core.Hash { return consensus.DemoAccount(i) }

func pay(from, to int, amount, fee, seq uint64) core.Tx {
	return core.Tx{Sender: acct(from), Recipient: acct(to), Amount: amount, Fee: fee, Seq: seq}
}

// peer is a node.Broadcaster that relays nothing and fetches blocks
// directly from another node, so a test can make a node switch forks.
type peer struct {
	mu  sync.Mutex
	src *node.Node
}

func (p *peer) BroadcastBlock(core.Block) {}

func (p *peer) FetchBlocks(_ string, loc []consensus.LocatorEntry) (uint64, []core.Block, error) {
	p.mu.Lock()
	src := p.src
	p.mu.Unlock()
	if src == nil {
		return 0, nil, errors.New("no peer")
	}
	return src.BlocksAfter(loc)
}

func newNode(t *testing.T, p consensus.Params) (*node.Node, *peer) {
	t.Helper()
	pr := &peer{}
	n, err := node.New(p, pr, node.Options{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Stop)
	return n, pr
}

// mineHeader returns a valid successor of prev at testDifficulty with the
// given Merkle root.
func mineHeader(t *testing.T, prev core.Header, root core.Hash) core.Header {
	t.Helper()
	h := core.Header{Version: 1, Prev: core.HeaderHash(prev), MerkleRoot: root,
		Timestamp: prev.Timestamp + 1, Difficulty: testDifficulty}
	r, err := miner.Mine(ctx, h, testDifficulty, 2)
	if err != nil {
		t.Fatal(err)
	}
	h.Nonce = r.Nonce
	return h
}

// mineHeaders returns k valid headers extending prev (header-only chains;
// salt makes different branches differ).
func mineHeaders(t *testing.T, prev core.Header, k int, salt byte) []core.Header {
	t.Helper()
	out := make([]core.Header, 0, k)
	for i := range k {
		h := mineHeader(t, prev, core.SHA256([]byte{salt, byte(i)}))
		out = append(out, h)
		prev = h
	}
	return out
}

// mine returns a valid block with txs on prev.
func mine(t *testing.T, prev core.Header, txs []core.Tx) core.Block {
	t.Helper()
	return core.Block{Header: mineHeader(t, prev, core.MerkleRoot(core.TxIDs(txs))), Txs: txs}
}

// grow appends k blocks to n, each with one payment by sender.
func grow(t *testing.T, n *node.Node, k, sender int) []core.Block {
	t.Helper()
	var out []core.Block
	for range k {
		_, tip := n.Tip()
		_, seq := n.Account(acct(sender))
		b := mine(t, tip, []core.Tx{pay(sender, 7, 1, 1, seq)})
		if err := n.ReceiveBlock(b, "test:1"); err != nil {
			t.Fatalf("grow: %v", err)
		}
		out = append(out, b)
	}
	return out
}

// front is an HTTP server whose behaviour a test can switch at any time
// (usually a real node's api.Server). It counts requests per "METHOD path"
// and records every locator it is sent.
type front struct {
	addr string
	ts   *httptest.Server

	mu   sync.Mutex
	h    http.Handler
	hits map[string]int
	locs []string
}

func newFront(t *testing.T, h http.Handler) *front {
	t.Helper()
	f := &front{h: h, hits: make(map[string]int)}
	f.ts = httptest.NewServer(f)
	t.Cleanup(f.ts.Close)
	f.addr = strings.TrimPrefix(f.ts.URL, "http://")
	return f
}

func (f *front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	key := r.Method + " " + r.URL.Path
	if strings.HasPrefix(r.URL.Path, "/proof/") {
		key = r.Method + " /proof"
	}
	f.hits[key]++
	if loc := r.URL.Query().Get("locator"); loc != "" {
		f.locs = append(f.locs, loc)
	}
	h := f.h
	f.mu.Unlock()
	h.ServeHTTP(w, r)
}

func (f *front) set(h http.Handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.h = h
}

func (f *front) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[key]
}

func (f *front) locators() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.locs)
}

// reject answers every request with status and an ErrorResponse with code
// (a plain-text body if code is empty, like the router's 404/405), and with
// the load header load if it is not empty.
func reject(status int, code, load string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if load != "" {
			w.Header().Set(routing.LoadHeader, load)
		}
		if code == "" {
			http.Error(w, http.StatusText(status), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(api.ErrorResponse{Code: code, Error: "scripted " + code})
	})
}

// malformed answers 200 with a body that is not JSON.
func malformed() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "{")
	})
}

// drop closes the connection without answering (a connection failure).
func drop() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	})
}

// hang never answers until the client gives up or the test ends.
func hang(t *testing.T) http.Handler {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) }) // runs before the servers close
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	})
}

// headersAnswer answers GET /headers?locator= with a fixed fork and headers.
func headersAnswer(fork uint64, hs []core.Header) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.HeadersAfterResponse{Fork: fork, Headers: api.HeadersToJSON(hs)})
	})
}

// closedAddr returns a host:port on which nothing listens.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// newLight returns a light client for p over the nodes addrs, and the
// ring's candidate order for its key (order[0] is the owner).
func newLight(t *testing.T, p consensus.Params, opt routing.Options, addrs ...string) (*LightClient, []string) {
	t.Helper()
	ring := routing.New(64, addrs...)
	ac := api.NewClient()
	t.Cleanup(ac.CloseIdleConnections)
	return New(clientID, NewHeaderChain(p), routing.NewSelector(ring, opt), ac), ring.Candidates(clientID)
}

var ringOnly = routing.Options{Strategy: routing.StrategyRing}

// pair returns two fronts in the ring order of a light client over both:
// the owner of clientID and the other one.
func pair(t *testing.T, p consensus.Params, opt routing.Options, a, b *front) (lc *LightClient, owner, other *front) {
	t.Helper()
	lc, order := newLight(t, p, opt, a.addr, b.addr)
	if order[0] == a.addr {
		return lc, a, b
	}
	return lc, b, a
}

func picked(t *testing.T, lc *LightClient) string {
	t.Helper()
	n, err := lc.sel.Pick(clientID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func sameHeaders(a, b []core.Header) bool { return slices.Equal(a, b) }

// ---- Header store and synchronisation (3.6) ----

// TestHeaderChainGenesis: a new store holds exactly the network's genesis,
// and its locator is D2's locator of that one-block chain.
func TestHeaderChainGenesis(t *testing.T) {
	p := testParams()
	hc := NewHeaderChain(p)
	if h, tip := hc.Tip(); h != 0 || tip != p.Genesis || hc.Height() != 0 {
		t.Fatalf("tip %d %v", h, tip)
	}
	if g, ok := hc.Header(0); !ok || g != p.Genesis {
		t.Fatal("no genesis")
	}
	if _, ok := hc.Header(1); ok {
		t.Fatal("header above the tip")
	}
	want := []consensus.LocatorEntry{{Height: 0, Hash: core.HeaderHash(p.Genesis)}}
	if got := hc.Locator(); !slices.Equal(got, want) {
		t.Fatalf("locator %v, want %v", got, want)
	}
}

// TestHeaderSync: catching up from genesis, then incrementally, one
// locator request each; the locator sent is D2's BuildLocator over the
// client's own headers; nothing new changes nothing.
func TestHeaderSync(t *testing.T) {
	p := testParams()
	n, _ := newNode(t, p)
	grow(t, n, 25, 1)
	f := newFront(t, api.NewServer(n, nil))
	hc := NewHeaderChain(p)
	ac := api.NewClient()

	res, err := hc.Sync(ctx, ac, f.addr)
	if err != nil {
		t.Fatal(err)
	}
	if res != (SyncResult{Node: f.addr, Fork: 0, Received: 25, Adopted: true, Height: 25}) {
		t.Fatalf("first sync: %+v", res)
	}
	if !sameHeaders(hc.Headers(), n.Headers(0)) {
		t.Fatal("headers differ from the node's")
	}

	grow(t, n, 3, 1)
	own := hc.Headers()
	want := api.FormatLocator(consensus.BuildLocator(25, func(h uint64) core.Hash { return core.HeaderHash(own[h]) }))
	if got := api.FormatLocator(hc.Locator()); got != want {
		t.Fatalf("Locator() is not BuildLocator over own headers")
	}
	res, err = hc.Sync(ctx, ac, f.addr)
	if err != nil || res != (SyncResult{Node: f.addr, Fork: 25, Received: 3, Adopted: true, Height: 28}) {
		t.Fatalf("incremental sync: %+v %v", res, err)
	}
	if locs := f.locators(); len(locs) != 2 || locs[1] != want {
		t.Fatalf("locator sent %q, want %q", locs, want)
	}
	if !sameHeaders(hc.Headers(), n.Headers(0)) {
		t.Fatal("headers differ after incremental sync")
	}

	res, err = hc.Sync(ctx, ac, f.addr)
	if err != nil || res != (SyncResult{Node: f.addr, Fork: 28, Received: 0, Adopted: false, Height: 28}) {
		t.Fatalf("idle sync: %+v %v", res, err)
	}
	if c := f.count("GET /headers"); c != 3 {
		t.Fatalf("%d header requests, want 3 (one per sync)", c)
	}
}

// TestRejectedSegments: every invalid answer is rejected with the right
// error and leaves the store exactly as it was; a valid chain that is not
// longer is kept out without an error; a longer valid one is adopted.
func TestRejectedSegments(t *testing.T) {
	p := testParams()
	n, _ := newNode(t, p)
	grow(t, n, 25, 1)
	hc := NewHeaderChain(p)
	ac := api.NewClient()
	if _, err := hc.Sync(ctx, ac, newFront(t, api.NewServer(n, nil)).addr); err != nil {
		t.Fatal(err)
	}
	before := hc.Headers()
	ext := mineHeaders(t, before[25], 3, 1) // a valid extension 26..28

	badWork := slices.Clone(ext)
	for badWork[2].Nonce = 0; core.PowCheck(core.HeaderHash(badWork[2]), testDifficulty); badWork[2].Nonce++ {
	}
	badLink := slices.Clone(ext)
	badLink[0].Prev[0] ^= 1
	midLink := slices.Clone(ext)
	midLink[1].Prev[31] ^= 1
	badDiff := slices.Clone(ext)
	badDiff[1].Difficulty = testDifficulty - 1
	pg := testParams()
	pg.Genesis.Timestamp++ // another network's genesis
	otherGenesis := mineHeaders(t, pg.Genesis, 30, 2)

	cases := []struct {
		name string
		fork uint64
		hs   []core.Header
		want []error
	}{
		{"fork point not in the locator", 15, mineHeaders(t, before[15], 12, 3), []error{ErrBadFork}},
		{"fork point above own tip", 26, ext[1:], []error{ErrBadFork}},
		{"first header does not link to own header", 25, badLink, []error{ErrInvalidSegment, consensus.ErrLinkage}},
		{"broken link inside the segment", 25, midLink, []error{ErrInvalidSegment, consensus.ErrLinkage}},
		{"insufficient proof of work", 25, badWork, []error{ErrInvalidSegment, consensus.ErrWork}},
		{"other difficulty", 25, badDiff, []error{ErrInvalidSegment, consensus.ErrDifficulty}},
		{"chain from a different genesis", 0, otherGenesis, []error{ErrInvalidSegment, consensus.ErrLinkage}},
	}
	f := newFront(t, nil)
	for _, c := range cases {
		f.set(headersAnswer(c.fork, c.hs))
		res, err := hc.Sync(ctx, ac, f.addr)
		for _, w := range c.want {
			if !errors.Is(err, w) {
				t.Errorf("%s: %v, want %v", c.name, err, w)
			}
		}
		if res.Adopted || !sameHeaders(hc.Headers(), before) {
			t.Fatalf("%s: store changed", c.name)
		}
	}

	// Valid, but not longer: kept out, no error (equal length, then shorter).
	for _, k := range []int{5, 3} {
		f.set(headersAnswer(20, mineHeaders(t, before[20], k, byte(10+k))))
		res, err := hc.Sync(ctx, ac, f.addr)
		if err != nil || res.Adopted || !sameHeaders(hc.Headers(), before) {
			t.Fatalf("fork of %d from 20: %+v %v", k, res, err)
		}
	}
	// Valid and longer: the store switches to it above the fork point.
	fork := mineHeaders(t, before[20], 6, 20)
	f.set(headersAnswer(20, fork))
	res, err := hc.Sync(ctx, ac, f.addr)
	if err != nil || !res.Adopted || res.Height != 26 {
		t.Fatalf("longer fork: %+v %v", res, err)
	}
	if got := hc.Headers(); !sameHeaders(got[:21], before[:21]) || !sameHeaders(got[21:], fork) {
		t.Fatal("store is not own headers to the fork point plus the new segment")
	}
}

// TestSyncSerialised: a second synchronisation waits until the first has
// applied its answer, so its locator describes the store it will change.
// Interleaved, the second answer (validated against the old store) would be
// written over the first one's fork and leave an unlinked chain.
func TestSyncSerialised(t *testing.T) {
	p := testParams()
	hc := NewHeaderChain(p)
	base := mineHeaders(t, p.Genesis, 3, 1)
	if _, err := hc.sync(ctx, func(context.Context, []consensus.LocatorEntry) (uint64, []core.Header, error) {
		return 0, base, nil
	}); err != nil {
		t.Fatal(err)
	}
	forkA := mineHeaders(t, base[2], 2, 2) // heights 4, 5 on top of 3
	forkB := mineHeaders(t, base[1], 4, 3) // heights 3..6 on top of 2
	entered := make(chan []consensus.LocatorEntry, 2)
	release := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		_, err := hc.sync(ctx, func(_ context.Context, loc []consensus.LocatorEntry) (uint64, []core.Header, error) {
			entered <- loc
			<-release
			return 3, forkA, nil
		})
		errs <- err
	}()
	<-entered
	go func() {
		_, err := hc.sync(ctx, func(_ context.Context, loc []consensus.LocatorEntry) (uint64, []core.Header, error) {
			entered <- loc
			return 2, forkB, nil
		})
		errs <- err
	}()
	select {
	case <-entered:
		t.Fatal("a second sync ran while the first was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if loc := <-entered; loc[0].Height != 5 {
		t.Fatalf("second locator starts at %d, want 5 (after the first sync)", loc[0].Height)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	got := hc.Headers()
	if err := consensus.CheckHeaderChain(p, got); err != nil || !sameHeaders(got[3:], forkB) {
		t.Fatalf("final store not genesis..2 + forkB: %v", err)
	}
}

// TestDifferentGenesis: a node of another network answers 409, which is
// returned typed; the store keeps only its own genesis.
func TestDifferentGenesis(t *testing.T) {
	p := testParams()
	pg := testParams()
	pg.Genesis.Timestamp++
	n, _ := newNode(t, pg)
	grow(t, n, 3, 1)
	f := newFront(t, api.NewServer(n, nil))
	lc, _ := newLight(t, p, ringOnly, f.addr)
	_, err := lc.Sync(ctx)
	var ae *api.APIError
	if !errors.Is(err, consensus.ErrGenesisMismatch) || !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("got %v, want 409 genesis_mismatch", err)
	}
	if lc.Headers().Height() != 0 || !sameHeaders(lc.Headers().Headers(), []core.Header{p.Genesis}) {
		t.Fatal("store changed")
	}
}

// TestForkSync: the node the client syncs from switches to a longer fork;
// the client follows in one locator request and adopts the new chain; an
// invalid segment offered afterwards changes nothing.
func TestForkSync(t *testing.T) {
	p := testParams()
	a, aPeer := newNode(t, p)
	b, _ := newNode(t, p)
	for _, blk := range grow(t, a, 3, 1) {
		if err := b.ReceiveBlock(blk, "test:1"); err != nil {
			t.Fatal(err)
		}
	}
	grow(t, a, 3, 1) // a: 0..6
	grow(t, b, 6, 2) // b: 0..3 shared, then 4..9 of its own
	f := newFront(t, api.NewServer(a, nil))
	lc, _ := newLight(t, p, ringOnly, f.addr)
	if res, err := lc.Sync(ctx); err != nil || res.Height != 6 || res.Node != f.addr {
		t.Fatalf("initial sync: %+v %v", res, err)
	}

	aPeer.mu.Lock()
	aPeer.src = b
	aPeer.mu.Unlock()
	if err := a.Sync("b"); err != nil { // the node switches to b's chain
		t.Fatal(err)
	}
	if h, _ := a.Tip(); h != 9 {
		t.Fatalf("node did not switch: height %d", h)
	}

	reqs := f.count("GET /headers")
	res, err := lc.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.count("GET /headers") - reqs; got != 1 {
		t.Fatalf("%d header requests, want 1", got)
	}
	// Own locator 6,5,4,3,2,1,0: the highest shared height is 3.
	if res.Fork != 3 || res.Received != 6 || !res.Adopted || res.Height != 9 {
		t.Fatalf("fork sync: %+v", res)
	}
	if !sameHeaders(lc.Headers().Headers(), b.Headers(0)) {
		t.Fatal("client is not on the new chain")
	}

	before := lc.Headers().Headers()
	bad := mineHeaders(t, before[3], 8, 9) // longer than 9 if it were valid
	bad[4].Prev[5] ^= 1
	f.set(headersAnswer(3, bad))
	if _, err := lc.Sync(ctx); !errors.Is(err, ErrInvalidSegment) {
		t.Fatalf("invalid longer segment: %v", err)
	}
	if !sameHeaders(lc.Headers().Headers(), before) {
		t.Fatal("store changed by an invalid segment")
	}
}

// ---- SPV proofs (3.7) ----

// confirmed returns a node holding block 1 with txs (and two blocks above
// it), a front for it, and a light client synced to it.
func confirmed(t *testing.T, txs []core.Tx) (*node.Node, *front, *LightClient) {
	t.Helper()
	p := testParams()
	n, _ := newNode(t, p)
	f := newFront(t, api.NewServer(n, nil))
	lc, _ := newLight(t, p, ringOnly, f.addr)
	for _, tx := range txs {
		sub, err := lc.Submit(ctx, tx)
		if err != nil || sub.TxID != core.TxID(tx) || sub.Node != f.addr {
			t.Fatalf("submit: %+v %v", sub, err)
		}
	}
	_, tip := n.Tip()
	if err := n.ReceiveBlock(mine(t, tip, txs), "test:1"); err != nil {
		t.Fatal(err)
	}
	grow(t, n, 2, 6)
	return n, f, lc
}

var fiveTxs = []core.Tx{pay(1, 7, 10, 1, 0), pay(2, 7, 20, 2, 0), pay(3, 7, 30, 3, 0), pay(4, 7, 40, 4, 0), pay(5, 7, 50, 5, 0)}

// TestProveGenuine: every transaction of a block is proven against the
// client's own header, with ⌈log₂ 5⌉ = 3 steps; before the client has the
// header the proof is refused; an unknown txid is the node's 404 answer.
func TestProveGenuine(t *testing.T) {
	_, f, lc := confirmed(t, fiveTxs)
	if _, err := lc.Prove(ctx, fiveTxs[0]); !errors.Is(err, ErrUnknownHeight) {
		t.Fatalf("before sync: %v, want ErrUnknownHeight", err)
	}
	if _, err := lc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tx := range fiveTxs {
		v, err := lc.Prove(ctx, tx)
		if err != nil || v != (Verified{TxID: core.TxID(tx), Height: 1, Node: f.addr}) {
			t.Fatalf("prove: %+v %v", v, err)
		}
		pr, err := lc.api.Proof(ctx, f.addr, core.TxID(tx))
		if err != nil || len(pr.Proof) != 3 {
			t.Fatalf("proof of %d steps (%v), want 3", len(pr.Proof), err)
		}
	}
	_, err := lc.Prove(ctx, pay(1, 7, 99, 1, 1))
	var ae *api.APIError
	if !errors.Is(err, core.ErrNotFound) || !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
		t.Fatalf("unknown tx: %v", err)
	}
}

// TestProofTampering: every tampered proof is rejected by the local check.
func TestProofTampering(t *testing.T) {
	_, f, lc := confirmed(t, fiveTxs)
	if _, err := lc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	hc := lc.Headers()
	tx := fiveTxs[2]
	pr, err := lc.api.Proof(ctx, f.addr, core.TxID(tx))
	if err != nil || VerifyProof(hc, tx, pr) != nil {
		t.Fatalf("genuine proof: %v %v", err, VerifyProof(hc, tx, pr))
	}
	clone := func() node.ProofResult {
		q := pr
		q.Proof = slices.Clone(pr.Proof)
		return q
	}
	expect := func(what string, want error, want2 core.Tx, q node.ProofResult) {
		t.Helper()
		if err := VerifyProof(hc, want2, q); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", what, err, want)
		}
	}

	// A tampered transaction field: the proof is for other bytes than the
	// transaction the client asked about.
	fields := []func(*core.Tx){
		func(x *core.Tx) { x.Sender[0] ^= 1 },
		func(x *core.Tx) { x.Recipient[31] ^= 1 },
		func(x *core.Tx) { x.Amount++ },
		func(x *core.Tx) { x.Fee++ },
		func(x *core.Tx) { x.Seq++ },
	}
	for _, mut := range fields {
		q := clone()
		mut(&q.Tx)
		expect("tampered tx field in the answer", ErrTxMismatch, tx, q)
		// Even if the client asked about the tampered transaction, the leaf
		// it computes (M-R7) is not in the block.
		expect("tampered tx asked about", ErrInvalidProof, q.Tx, q)
	}
	for i := range pr.Proof {
		q := clone()
		q.Proof[i].Sibling[i%32] ^= 0x80
		expect("tampered sibling", ErrInvalidProof, tx, q)
		q = clone()
		q.Proof[i].Side ^= 1
		expect("tampered direction", ErrInvalidProof, tx, q)
	}
	q := clone()
	q.Proof = q.Proof[:2]
	expect("truncated proof", ErrInvalidProof, tx, q)
	q = clone()
	q.Proof = append(q.Proof, q.Proof[0])
	expect("extended proof", ErrInvalidProof, tx, q)
	q = clone()
	q.Height = hc.Height() + 1
	expect("height the client does not have", ErrUnknownHeight, tx, q)
	q = clone()
	q.Height = 2
	expect("other height", ErrInvalidProof, tx, q)
	q = clone()
	q.Height = 0
	expect("genesis", ErrInvalidProof, tx, q)

	// The same over the network: a node serving a tampered proof.
	f.set(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := clone()
		q.Proof[1].Sibling[3] ^= 1
		json.NewEncoder(w).Encode(api.ProofToJSON(q))
	}))
	if _, err := lc.Prove(ctx, tx); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("tampered proof over HTTP: %v", err)
	}
}

// TestOwnRootOnly (M-R8): a node on another chain proves a transaction
// that is valid against its own block, and even adds "its" Merkle root to
// the answer; the client checks only against its own header and rejects.
func TestOwnRootOnly(t *testing.T) {
	p := testParams()
	honest, _ := newNode(t, p)
	liar, _ := newNode(t, p)
	_, ht := honest.Tip()
	if err := honest.ReceiveBlock(mine(t, ht, []core.Tx{pay(1, 7, 5, 1, 0)}), "test:1"); err != nil {
		t.Fatal(err)
	}
	fake := pay(2, 7, 900, 1, 0)
	_, lt := liar.Tip()
	if err := liar.ReceiveBlock(mine(t, lt, []core.Tx{fake, pay(3, 7, 1, 1, 0)}), "test:1"); err != nil {
		t.Fatal(err)
	}
	ac := api.NewClient()
	hf := newFront(t, api.NewServer(honest, nil))
	lf := newFront(t, api.NewServer(liar, nil))
	own := NewHeaderChain(p)
	if _, err := own.Sync(ctx, ac, hf.addr); err != nil {
		t.Fatal(err)
	}
	theirs := NewHeaderChain(p)
	if _, err := theirs.Sync(ctx, ac, lf.addr); err != nil {
		t.Fatal(err)
	}
	pr, err := ac.Proof(ctx, lf.addr, core.TxID(fake))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyProof(theirs, fake, pr); err != nil {
		t.Fatalf("control: the proof is valid against the liar's own root: %v", err)
	}
	if err := VerifyProof(own, fake, pr); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("against the client's own header: %v, want ErrInvalidProof", err)
	}

	// The liar adds a root field to its answer; the client never reads it.
	liarRoot, _ := theirs.Header(1)
	lf.set(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		api.NewServer(liar, nil).ServeHTTP(rec, r)
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Error(err)
		}
		m["merkle_root"] = liarRoot.MerkleRoot.String()
		m["root"] = liarRoot.MerkleRoot.String()
		json.NewEncoder(w).Encode(m)
	}))
	lc := New(clientID, own, routing.NewSelector(routing.New(64, lf.addr), ringOnly), ac)
	if _, err := lc.Prove(ctx, fake); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("answer with a server root: %v, want ErrInvalidProof", err)
	}
}

// ---- Selector integration: application answers versus node failures ----

// TestRejectionIsAnAnswer: an HTTP 400 rejection from a real node comes
// back as the original typed error; the node's failure count is reset, not
// increased; its health is unchanged; no other node is tried and the
// transaction is not sent elsewhere. Genuine connection failures, in
// contrast, are counted and fail over.
func TestRejectionIsAnAnswer(t *testing.T) {
	p := testParams()
	n1, _ := newNode(t, p)
	n2, _ := newNode(t, p)
	f1 := newFront(t, api.NewServer(n1, nil))
	f2 := newFront(t, api.NewServer(n2, nil))
	nodes := map[string]*node.Node{f1.addr: n1, f2.addr: n2}
	lc, owner, other := pair(t, p, ringOnly, f1, f2)
	ownerNode, otherNode := nodes[owner.addr], nodes[other.addr]

	if s, err := lc.Submit(ctx, pay(1, 7, 10, 1, 0)); err != nil || s.Node != owner.addr {
		t.Fatalf("accepted: %+v %v", s, err)
	}
	rejections := []struct {
		tx       core.Tx
		code     string
		sentinel error
	}{
		{pay(1, 7, 10, 1, 1), api.CodeSenderPending, mempool.ErrSenderPending},
		{pay(1, 7, 10, 1, 0), api.CodeDuplicate, mempool.ErrDuplicate},
		{pay(2, 7, 2_000_000, 1, 0), api.CodeBalance, core.ErrBalance},
		{pay(3, 7, 1, 1, 5), api.CodeSeq, core.ErrSeq},
	}
	for _, r := range rejections {
		s, err := lc.Submit(ctx, r.tx)
		var ae *api.APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest || ae.Code != r.code || !errors.Is(err, r.sentinel) {
			t.Fatalf("%s: got %v", r.code, err)
		}
		if s.Node != owner.addr || s.TxID != core.TxID(r.tx) {
			t.Fatalf("%s: answered by %q", r.code, s.Node)
		}
	}
	if c := other.count("POST /tx"); c != 0 || otherNode.MempoolSize() != 0 {
		t.Fatalf("rejected transactions reached the other node (%d requests)", c)
	}
	if picked(t, lc) != owner.addr {
		t.Fatal("rejections changed the owner's health")
	}

	// drop → 1 failure; a rejection → counts as communication (reset to 0);
	// drop → 1 failure: still up. Had the rejection been a failure (or been
	// ignored), the owner would now have 2 failures and be down.
	owner.set(drop())
	if s, err := lc.Submit(ctx, pay(3, 7, 1, 1, 0)); err != nil || s.Node != other.addr {
		t.Fatalf("failover after a dropped connection: %+v %v", s, err)
	}
	owner.set(api.NewServer(ownerNode, nil))
	if _, err := lc.Submit(ctx, pay(1, 7, 10, 1, 1)); !errors.Is(err, mempool.ErrSenderPending) {
		t.Fatalf("rejection: %v", err)
	}
	owner.set(drop())
	if s, err := lc.Submit(ctx, pay(4, 7, 1, 1, 0)); err != nil || s.Node != other.addr {
		t.Fatalf("second failover: %+v %v", s, err)
	}
	if picked(t, lc) != owner.addr {
		t.Fatal("the rejection was counted as a failure")
	}
	// A further genuine failure: 2 consecutive → down.
	if s, err := lc.Submit(ctx, pay(5, 7, 1, 1, 0)); err != nil || s.Node != other.addr {
		t.Fatalf("third failover: %+v %v", s, err)
	}
	if picked(t, lc) != other.addr {
		t.Fatal("two connection failures did not mark the owner down")
	}

	// A rejection from a node that is down (tried as the last resort) is
	// returned, and does not bring the node back up (only a probe does).
	other.set(drop())
	owner.set(api.NewServer(ownerNode, nil))
	s, err := lc.Submit(ctx, pay(1, 7, 10, 1, 1))
	if !errors.Is(err, mempool.ErrSenderPending) || s.Node != owner.addr {
		t.Fatalf("rejection from a down node: %+v %v", s, err)
	}
	if picked(t, lc) != other.addr {
		t.Fatal("a rejection brought a down node back up")
	}
}

// TestRejectionLoadReport: the load header of a 400 answer is recorded
// (DR-13), and the send/report bookkeeping (k) stays consistent:
// RecordSend before the request, RecordReport (which resets k) after it.
// Estimates E = R + (n+k+1)·D/(n+1), with R = 1 µs and n = 0 here.
func TestRejectionLoadReport(t *testing.T) {
	p := testParams()
	now := time.Unix(1_800_000_000, 0)
	opt := routing.Options{Strategy: routing.StrategyP2C, Now: func() time.Time { return now }, IntN: func(int) int { return 0 }}
	lc, owner, other := pair(t, p, opt, newFront(t, nil), newFront(t, nil))
	other.set(reject(http.StatusInternalServerError, api.CodeInternal, "")) // must never be reached
	report := func(n string, d time.Duration) {
		lc.sel.RecordProbe(n, routing.ProbeResult{RTT: time.Microsecond, Report: routing.LoadReport{Delay: d}, HasReport: true}, nil)
	}
	submit := func(load string) {
		t.Helper()
		owner.set(reject(http.StatusBadRequest, api.CodeSenderPending, load))
		s, err := lc.Submit(ctx, pay(1, 7, 1, 1, 1))
		if !errors.Is(err, mempool.ErrSenderPending) || s.Node != owner.addr {
			t.Fatalf("submit: %+v %v", s, err)
		}
	}
	report(other.addr, 12*time.Millisecond)

	// The 400's report (50 ms) replaces the owner's 1 ms: E 50 > 12/0.8.
	report(owner.addr, time.Millisecond)
	if picked(t, lc) != owner.addr {
		t.Fatal("setup: owner not picked")
	}
	submit("d=50000,n=0")
	if picked(t, lc) != other.addr {
		t.Fatal("the load report of a 400 answer was not recorded")
	}

	// A 10 ms report resets k: E = 10 ms, the owner stays (12 ≥ 0.8·10).
	// With k = 1 left over, E would be 20 ms and the other node would win.
	report(owner.addr, time.Millisecond)
	submit("d=10000,n=0")
	if picked(t, lc) != owner.addr {
		t.Fatal("the report did not reset k")
	}

	// No (or an unusable) report: the rejected send still counts in k,
	// so E = (0+1+1)·10 ms = 20 ms and 12 < 0.8·20 → the other node.
	for _, load := range []string{"", "d=x,n=0"} {
		report(owner.addr, 10*time.Millisecond)
		submit(load)
		if picked(t, lc) != other.addr {
			t.Fatalf("load %q: the rejected send was not counted (k)", load)
		}
	}
	if c := other.count("POST /tx"); c != 0 {
		t.Fatalf("%d submissions reached the other node", c)
	}
}

// TestStatusClassification: each kind of answer or failure is treated by
// its meaning. 4xx answers and malformed 2xx answers are returned to the
// caller without failover; 5xx answers and connection failures fail over
// and count one failure.
func TestStatusClassification(t *testing.T) {
	p := testParams()
	var ae *api.APIError
	isStatus := func(status int, code string) func(error) bool {
		return func(err error) bool { return errors.As(err, &ae) && ae.Status == status && ae.Code == code }
	}
	is := func(target error) func(error) bool { return func(err error) bool { return errors.Is(err, target) } }
	cases := []struct {
		name     string
		h        http.Handler
		failover bool
		check    func(error) bool
	}{
		{"400 bad_request", reject(400, api.CodeBadRequest, ""), false, is(api.ErrMalformed)},
		{"400 duplicate", reject(400, api.CodeDuplicate, ""), false, is(mempool.ErrDuplicate)},
		{"400 sender_pending", reject(400, api.CodeSenderPending, ""), false, is(mempool.ErrSenderPending)},
		{"400 balance", reject(400, api.CodeBalance, ""), false, is(core.ErrBalance)},
		{"400 seq", reject(400, api.CodeSeq, ""), false, is(core.ErrSeq)},
		{"400 overflow", reject(400, api.CodeOverflow, ""), false, is(core.ErrOverflow)},
		{"404 plain (router)", reject(404, "", ""), false, isStatus(404, "")},
		{"405 plain (router)", reject(405, "", ""), false, isStatus(405, "")},
		{"409 genesis_mismatch", reject(409, api.CodeGenesisMismatch, ""), false, is(consensus.ErrGenesisMismatch)},
		{"200 malformed body", malformed(), false, is(api.ErrMalformed)},
		{"500 internal", reject(500, api.CodeInternal, ""), true, nil},
		{"502 sync_failed", reject(502, api.CodeSyncFailed, ""), true, nil},
		{"503 unavailable", reject(503, api.CodeUnavailable, ""), true, nil},
		{"connection dropped", drop(), true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, _ := newNode(t, p)
			lc, owner, other := pair(t, p, ringOnly, newFront(t, nil), newFront(t, nil))
			other.set(api.NewServer(n, nil))
			owner.set(c.h)
			tx := pay(1, 7, 1, 1, 0)
			s, err := lc.Submit(ctx, tx)
			if c.failover {
				if err != nil || s.Node != other.addr || other.count("POST /tx") != 1 || n.MempoolSize() != 1 {
					t.Fatalf("want failover: %+v %v", s, err)
				}
			} else {
				if !c.check(err) || s.Node != owner.addr || other.count("POST /tx") != 0 {
					t.Fatalf("want the answer returned without failover: %+v %v", s, err)
				}
			}
			if picked(t, lc) != owner.addr { // at most one failure: still up
				t.Fatal("owner marked down")
			}
		})
	}

	// GET requests are classified the same way.
	t.Run("GET", func(t *testing.T) {
		n, _ := newNode(t, p)
		grow(t, n, 2, 1)
		lc, owner, other := pair(t, p, ringOnly, newFront(t, nil), newFront(t, nil))
		other.set(api.NewServer(n, nil))
		owner.set(reject(404, api.CodeNotFound, ""))
		if v, err := lc.Prove(ctx, pay(1, 7, 1, 1, 0)); !errors.Is(err, core.ErrNotFound) || v.Node != owner.addr || other.count("GET /proof") != 0 {
			t.Fatalf("proof 404: %+v %v", v, err)
		}
		owner.set(reject(409, api.CodeGenesisMismatch, ""))
		if res, err := lc.Sync(ctx); !errors.Is(err, consensus.ErrGenesisMismatch) || res.Node != owner.addr || other.count("GET /headers") != 0 {
			t.Fatalf("headers 409: %+v %v", res, err)
		}
		owner.set(malformed())
		if res, err := lc.Sync(ctx); !errors.Is(err, api.ErrMalformed) || res.Height != 0 || other.count("GET /headers") != 0 {
			t.Fatalf("headers malformed: %+v %v", res, err)
		}
		owner.set(reject(503, api.CodeUnavailable, ""))
		if res, err := lc.Sync(ctx); err != nil || res.Node != other.addr || res.Height != 2 {
			t.Fatalf("headers 503: %+v %v", res, err)
		}
		if v, err := lc.Prove(ctx, pay(1, 7, 1, 1, 0)); v.Node != other.addr {
			t.Fatalf("proof 503: %+v %v", v, err) // the other node then answers (found or 404)
		}
		if picked(t, lc) != other.addr {
			t.Fatal("two 503 answers did not mark the owner down")
		}
	})
}

// TestFailover: through the selector, submissions, proofs and header syncs
// reach another node when the owner is stopped (503 from a real node) or
// unreachable (connection refused); refusals count as failures; when no
// node answers, the error is typed.
func TestFailover(t *testing.T) {
	p := testParams()
	var fs []*front
	var ns []*node.Node
	for range 3 {
		n, _ := newNode(t, p)
		ns = append(ns, n)
		fs = append(fs, newFront(t, api.NewServer(n, nil)))
	}
	lc, order := newLight(t, p, ringOnly, fs[0].addr, fs[1].addr, fs[2].addr)
	byAddr := func(a string) (*front, *node.Node) {
		for i, f := range fs {
			if f.addr == a {
				return f, ns[i]
			}
		}
		t.Fatal("unknown node")
		return nil, nil
	}
	ownerF, ownerN := byAddr(order[0])
	secondF, secondN := byAddr(order[1])
	_, thirdN := byAddr(order[2])

	tx := pay(1, 7, 10, 1, 0)
	ownerN.Stop() // the owner's server now answers 503 unavailable
	s, err := lc.Submit(ctx, tx)
	if err != nil || s.Node != secondF.addr || secondN.MempoolSize() != 1 || thirdN.MempoolSize() != 0 {
		t.Fatalf("submit with the owner stopped: %+v %v", s, err)
	}
	_, tip := secondN.Tip()
	if err := secondN.ReceiveBlock(mine(t, tip, []core.Tx{tx}), "test:1"); err != nil {
		t.Fatal(err)
	}
	ownerF.ts.Close() // now the owner refuses connections
	if res, err := lc.Sync(ctx); err != nil || res.Node != secondF.addr || res.Height != 1 {
		t.Fatalf("sync with the owner unreachable: %+v %v", res, err)
	}
	if picked(t, lc) != secondF.addr { // 503 + refused = 2 failures
		t.Fatal("owner not marked down")
	}
	if v, err := lc.Prove(ctx, tx); err != nil || v.Node != secondF.addr || v.Height != 1 {
		t.Fatalf("prove: %+v %v", v, err)
	}

	// Every node unreachable: ErrNoNode, with each node's error kept.
	lc2, _ := newLight(t, p, ringOnly, closedAddr(t), closedAddr(t))
	_, err = lc2.Submit(ctx, tx)
	var ne net.Error
	if !errors.Is(err, ErrNoNode) || !errors.As(err, &ne) {
		t.Fatalf("no node: %v", err)
	}
	lc3, _ := newLight(t, p, ringOnly) // no nodes at all

	if _, err := lc3.Submit(ctx, tx); !errors.Is(err, routing.ErrEmpty) {
		t.Fatalf("empty ring: %v", err)
	}
	// A cancelled caller blames no node.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := lc.Submit(cctx, pay(2, 7, 1, 1, 0)); !errors.Is(err, context.Canceled) || errors.Is(err, ErrNoNode) {
		t.Fatalf("cancelled: %v", err)
	}
}

// TestTimeoutFailover: a node that accepts the connection but never
// answers fails the request after the fixed RequestTimeout (5 s), and the
// selector moves on; the timeout counts as a failure and stays typed.
func TestTimeoutFailover(t *testing.T) {
	p := testParams()
	t.Run("owner hangs", func(t *testing.T) {
		t.Parallel()
		n, _ := newNode(t, p)
		lc, owner, other := pair(t, p, ringOnly, newFront(t, nil), newFront(t, nil))
		other.set(api.NewServer(n, nil))
		owner.set(hang(t))
		start := time.Now()
		s, err := lc.Submit(ctx, pay(1, 7, 1, 1, 0))
		el := time.Since(start)
		if err != nil || s.Node != other.addr || n.MempoolSize() != 1 {
			t.Fatalf("%+v %v", s, err)
		}
		if el < routing.RequestTimeout-100*time.Millisecond || el > routing.RequestTimeout+2*time.Second {
			t.Fatalf("failover after %v, want about %v", el, routing.RequestTimeout)
		}
		if picked(t, lc) != owner.addr { // one failure: still up
			t.Fatal("one timeout marked the owner down")
		}
	})
	t.Run("no node answers", func(t *testing.T) {
		t.Parallel()
		lc, owner, other := pair(t, p, ringOnly, newFront(t, nil), newFront(t, nil))
		owner.set(hang(t))
		other.ts.Close()
		_, err := lc.Submit(ctx, pay(1, 7, 1, 1, 0))
		if !errors.Is(err, ErrNoNode) || !api.IsTimeout(err) {
			t.Fatalf("got %v, want ErrNoNode with a timeout", err)
		}
		if !strings.Contains(err.Error(), owner.addr) || !strings.Contains(err.Error(), other.addr) {
			t.Fatalf("error does not name both nodes: %v", err)
		}
	})
}

// TestConcurrentUse: syncs, proofs, submissions and reads from many
// goroutines while the node's chain grows (run under -race).
func TestConcurrentUse(t *testing.T) {
	n, _, lc := confirmed(t, fiveTxs)
	var wg sync.WaitGroup
	errc := make(chan error, 64)
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 15 {
				if _, err := lc.Sync(ctx); err != nil {
					errc <- err
					return
				}
				lc.Headers().Locator()
				lc.Headers().Tip()
				if _, err := lc.Prove(ctx, fiveTxs[(g+i)%5]); err != nil {
					errc <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := lc.Submit(ctx, pay(0, 7, 1, 1, 0))
		if err != nil {
			errc <- err
		}
	}()
	grow(t, n, 5, 6)
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	if res, err := lc.Sync(ctx); err != nil || res.Height != 8 {
		t.Fatalf("final sync: %+v %v", res, err)
	}
	if !sameHeaders(lc.Headers().Headers(), n.Headers(0)) {
		t.Fatal("headers differ from the node's")
	}
}
