package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

func testParams() consensus.Params {
	p := consensus.Default()
	p.Difficulty = testDifficulty
	return p
}

func acct(i int) core.Hash { return consensus.DemoAccount(i) }

func pay(from, to int, amount, fee, seq uint64) core.Tx {
	return core.Tx{Sender: acct(from), Recipient: acct(to), Amount: amount, Fee: fee, Seq: seq}
}

func newNode(t *testing.T, b node.Broadcaster) *node.Node {
	t.Helper()
	n, err := node.New(testParams(), b, node.Options{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Stop)
	return n
}

// buildBlock mines a block with txs on prev, outside any node.
func buildBlock(t *testing.T, prev core.Header, txs []core.Tx) core.Block {
	t.Helper()
	h := core.Header{Version: 1, Prev: core.HeaderHash(prev), MerkleRoot: core.MerkleRoot(core.TxIDs(txs)),
		Timestamp: time.Now().Unix(), Difficulty: testDifficulty}
	r, err := miner.Mine(context.Background(), h, testDifficulty, 2)
	if err != nil {
		t.Fatal(err)
	}
	h.Nonce = r.Nonce
	return core.Block{Header: h, Txs: txs}
}

// grow appends k blocks to n, each with one payment by sender, through
// ReceiveBlock (so a node with a PeerTransport relays them).
func grow(t *testing.T, n *node.Node, k, sender int) []core.Block {
	t.Helper()
	var out []core.Block
	for range k {
		_, tip := n.Tip()
		_, seq := n.Account(acct(sender))
		b := buildBlock(t, tip, []core.Tx{pay(sender, 7, 1, 1, seq)})
		if err := n.ReceiveBlock(b, "test:1"); err != nil {
			t.Fatalf("grow: %v", err)
		}
		out = append(out, b)
	}
	return out
}

// serve starts an HTTP server for h and returns its host:port.
func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return strings.TrimPrefix(ts.URL, "http://")
}

// raw sends a request without the Client and returns status, header, body.
func raw(t *testing.T, method, addr, path, body string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

// wantError checks an ErrorResponse answer.
func wantError(t *testing.T, what string, status int, data []byte, wantStatus int, wantCode string) {
	t.Helper()
	var er ErrorResponse
	if err := json.Unmarshal(data, &er); err != nil || status != wantStatus || er.Code != wantCode || er.Error == "" {
		t.Errorf("%s: got %d %s, want %d %s", what, status, data, wantStatus, wantCode)
	}
}

// wantAPIError checks a Client error: status, code and sentinel.
func wantAPIError(t *testing.T, what string, err error, status int, code string, sentinel error) {
	t.Helper()
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != status || ae.Code != code {
		t.Errorf("%s: got %v, want HTTP %d %s", what, err, status, code)
		return
	}
	if sentinel != nil && !errors.Is(err, sentinel) {
		t.Errorf("%s: %v does not match %v", what, err, sentinel)
	}
}

func randHash(r *rand.Rand) (h core.Hash) {
	for i := range h {
		h[i] = byte(r.UintN(256))
	}
	return h
}

func randTx(r *rand.Rand) core.Tx {
	return core.Tx{Sender: randHash(r), Recipient: randHash(r), Amount: r.Uint64(), Fee: r.Uint64(), Seq: r.Uint64()}
}

func randHeader(r *rand.Rand) core.Header {
	return core.Header{Version: r.Uint32(), Prev: randHash(r), MerkleRoot: randHash(r),
		Timestamp: int64(r.Uint64()), Difficulty: r.Uint32(), Nonce: r.Uint64()}
}

// viaJSON marshals v and unmarshals it into out (a wire round trip).
func viaJSON(t *testing.T, v, out any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

// ---- DTOs ----

func TestDTORoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	edge := []core.Tx{{}, {Amount: math.MaxUint64, Fee: math.MaxUint64, Seq: math.MaxUint64}}
	for i := range 300 {
		tx := randTx(r)
		if i < len(edge) {
			tx = edge[i]
		}
		var j TxJSON
		viaJSON(t, TxToJSON(tx), &j)
		got, err := TxFromJSON(j)
		if err != nil || got != tx || core.TxID(got) != core.TxID(tx) {
			t.Fatalf("tx round trip: %+v -> %+v, %v", tx, got, err)
		}
		if len(j.Sender) != 64 || strings.ToLower(j.Sender) != j.Sender {
			t.Fatalf("hash not 64 lowercase hex digits: %q", j.Sender)
		}

		h := randHeader(r)
		if i == 0 {
			h = core.Header{Timestamp: math.MinInt64}
		}
		var hj HeaderJSON
		viaJSON(t, HeaderToJSON(h), &hj)
		gh, err := HeaderFromJSON(hj)
		if err != nil || gh != h || core.HeaderHash(gh) != core.HeaderHash(h) {
			t.Fatalf("header round trip: %+v -> %+v, %v", h, gh, err)
		}

		b := core.Block{Header: h}
		for range r.IntN(5) {
			b.Txs = append(b.Txs, randTx(r))
		}
		var bj BlockJSON
		viaJSON(t, BlockToJSON(b), &bj)
		gb, err := BlockFromJSON(bj)
		if err != nil || gb.Header != b.Header || len(gb.Txs) != len(b.Txs) {
			t.Fatalf("block round trip: %v", err)
		}
		for k := range b.Txs {
			if gb.Txs[k] != b.Txs[k] {
				t.Fatalf("block tx %d differs", k)
			}
		}
	}

	// An empty block encodes its transactions as [], never null.
	data, _ := json.Marshal(BlockToJSON(core.Block{}))
	if !bytes.Contains(data, []byte(`"txs":[]`)) {
		t.Errorf("empty block: %s", data)
	}
	data, _ = json.Marshal(HeadersFromResponse{Headers: HeadersToJSON(nil)})
	if !bytes.Contains(data, []byte(`"headers":[]`)) {
		t.Errorf("no headers: %s", data)
	}

	// A real proof survives the wire and still verifies.
	txs := []core.Tx{randTx(r), randTx(r), randTx(r), randTx(r), randTx(r)}
	ids := core.TxIDs(txs)
	root := core.MerkleRoot(ids)
	for i := range txs {
		p, err := core.MerkleProof(ids, uint64(i))
		if err != nil {
			t.Fatal(err)
		}
		var pj ProofResponse
		viaJSON(t, ProofToJSON(node.ProofResult{Height: 9, Tx: txs[i], Proof: p}), &pj)
		got, err := ProofFromJSON(pj)
		if err != nil || got.Height != 9 || got.Tx != txs[i] || len(got.Proof) != len(p) {
			t.Fatalf("proof round trip %d: %v", i, err)
		}
		if !core.MerkleVerify(root, core.TxID(got.Tx), got.Proof) {
			t.Fatalf("proof %d does not verify after the round trip", i)
		}
	}
}

func TestDTOMalformed(t *testing.T) {
	good := TxToJSON(pay(0, 1, 5, 1, 0))
	bads := []string{"", "00", strings.Repeat("0", 63), strings.Repeat("0", 65), strings.Repeat("g", 64), "0x" + strings.Repeat("0", 62)}
	for _, bad := range bads {
		j := good
		j.Sender = bad
		if _, err := TxFromJSON(j); !errors.Is(err, ErrMalformed) {
			t.Errorf("sender %q: %v", bad, err)
		}
		j = good
		j.Recipient = bad
		if _, err := TxFromJSON(j); !errors.Is(err, ErrMalformed) {
			t.Errorf("recipient %q: %v", bad, err)
		}
		hj := HeaderToJSON(core.Header{})
		hj.Prev = bad
		if _, err := HeaderFromJSON(hj); !errors.Is(err, ErrMalformed) {
			t.Errorf("prev %q: %v", bad, err)
		}
		hj = HeaderToJSON(core.Header{})
		hj.MerkleRoot = bad
		if _, err := HeaderFromJSON(hj); !errors.Is(err, ErrMalformed) {
			t.Errorf("merkle_root %q: %v", bad, err)
		}
	}
	// Upper-case hex is accepted.
	up := good
	up.Sender = strings.ToUpper(up.Sender)
	if tx, err := TxFromJSON(up); err != nil || tx.Sender != acct(0) {
		t.Errorf("upper-case hex: %v", err)
	}

	bj := BlockToJSON(core.Block{Txs: []core.Tx{{}, {}}})
	bj.Txs[1].Recipient = "zz"
	if _, err := BlockFromJSON(bj); !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), "tx 1") {
		t.Errorf("block with a bad tx: %v", err)
	}
	bj = BlockToJSON(core.Block{})
	bj.Header.Prev = "zz"
	if _, err := BlockFromJSON(bj); !errors.Is(err, ErrMalformed) {
		t.Errorf("block with a bad header: %v", err)
	}
	if _, err := HeadersFromJSON([]HeaderJSON{HeaderToJSON(core.Header{}), {Prev: "x"}}); !errors.Is(err, ErrMalformed) {
		t.Errorf("headers: %v", err)
	}
	if _, err := BlocksFromJSON([]BlockJSON{{}}); !errors.Is(err, ErrMalformed) {
		t.Errorf("blocks: %v", err)
	}

	step := ProofStepJSON{Sibling: core.Hash{}.String(), Side: "left"}
	for _, c := range []struct {
		name string
		p    ProofResponse
	}{
		{"bad side", ProofResponse{Tx: good, Proof: []ProofStepJSON{step, {Sibling: step.Sibling, Side: "up"}}}},
		{"empty side", ProofResponse{Tx: good, Proof: []ProofStepJSON{{Sibling: step.Sibling}}}},
		{"numeric side", ProofResponse{Tx: good, Proof: []ProofStepJSON{{Sibling: step.Sibling, Side: "1"}}}},
		{"bad sibling", ProofResponse{Tx: good, Proof: []ProofStepJSON{{Sibling: "ab", Side: "right"}}}},
		{"bad tx", ProofResponse{Tx: TxJSON{}, Proof: nil}},
		{"too long", ProofResponse{Tx: good, Proof: make([]ProofStepJSON, core.MaxProofSteps+1)}},
	} {
		if _, err := ProofFromJSON(c.p); !errors.Is(err, ErrMalformed) {
			t.Errorf("proof %s: %v", c.name, err)
		}
	}
	long := ProofResponse{Tx: good, Proof: make([]ProofStepJSON, core.MaxProofSteps)}
	for i := range long.Proof {
		long.Proof[i] = step
	}
	if _, err := ProofFromJSON(long); err != nil {
		t.Errorf("proof of MaxProofSteps steps: %v", err)
	}
}

func TestLocatorFormatParse(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	hashes := map[uint64]core.Hash{}
	hashAt := func(h uint64) core.Hash {
		if _, ok := hashes[h]; !ok {
			hashes[h] = randHash(r)
		}
		return hashes[h]
	}
	for _, tip := range []uint64{0, 1, 9, 10, 30, 1000, 1 << 40} {
		loc := consensus.BuildLocator(tip, hashAt)
		s := FormatLocator(loc)
		got, err := ParseLocator(s)
		if err != nil || len(got) != len(loc) {
			t.Fatalf("tip %d: %v", tip, err)
		}
		for i := range loc {
			if got[i] != loc[i] {
				t.Fatalf("tip %d entry %d: %+v != %+v", tip, i, got[i], loc[i])
			}
		}
	}
	if s := FormatLocator([]consensus.LocatorEntry{{Height: 5, Hash: core.Hash{0xab}}, {Height: 0}}); s != "5:ab"+strings.Repeat("0", 62)+",0:"+strings.Repeat("0", 64) {
		t.Errorf("format: %s", s)
	}

	z := strings.Repeat("0", 64)
	entries := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf("%d:%s", n-1-i, z)
		}
		return strings.Join(parts, ",")
	}
	if loc, err := ParseLocator(entries(consensus.MaxLocatorEntries)); err != nil || len(loc) != consensus.MaxLocatorEntries {
		t.Errorf("128 entries: %v", err)
	}
	for _, bad := range []string{"", "0", "0:", ":" + z, "x:" + z, "-1:" + z, "1.5:" + z, "0:" + z[:63],
		"0:" + z + "0", "0:" + strings.Repeat("g", 64), "1:" + z + ",", "1:" + z + ",,0:" + z, "1:" + z + ";0:" + z,
		"18446744073709551616:" + z, entries(consensus.MaxLocatorEntries + 1)} {
		if _, err := ParseLocator(bad); !errors.Is(err, consensus.ErrBadLocator) {
			t.Errorf("ParseLocator(%.40q): %v", bad, err)
		}
	}
}

// ---- Endpoints ----

func TestTipEndpoint(t *testing.T) {
	n := newNode(t, nil)
	grow(t, n, 3, 0)
	addr := serve(t, NewServer(n, nil))
	c := NewClient()
	ctx := context.Background()

	// No load report before the first POST /tx.
	info, err := c.Tip(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	h, hd := n.Tip()
	if info.Height != 3 || info.Height != h || info.Header != hd || info.HasReport || info.MempoolSize != 0 {
		t.Fatalf("tip: %+v", info)
	}
	if _, hdr, _ := raw(t, "GET", addr, "/tip", ""); hdr.Get(routing.LoadHeader) != "" {
		t.Errorf("load header before any POST /tx: %q", hdr.Get(routing.LoadHeader))
	}

	// mempool_size follows the mempool (the node is not mining).
	for i := 1; i <= 3; i++ {
		if _, err := c.SubmitTx(ctx, addr, pay(i, 7, 1, 1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	info, err = c.Tip(ctx, addr)
	if err != nil || info.MempoolSize != 3 || info.MempoolSize != n.MempoolSize() || !info.HasReport || info.Report.InFlight != 0 {
		t.Fatalf("tip after 3 submissions: %+v, %v", info, err)
	}

	// The exact JSON fields.
	_, hdr, data := raw(t, "GET", addr, "/tip", "")
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || len(m) != 3 || m["height"] == nil || m["header"] == nil || m["mempool_size"] == nil {
		t.Errorf("tip JSON: %s", data)
	}
	if hdr.Get("Content-Type") != "application/json" {
		t.Errorf("content type %q", hdr.Get("Content-Type"))
	}
}

func TestSubmitTx(t *testing.T) {
	n := newNode(t, nil)
	addr := serve(t, NewServer(n, nil))
	c := NewClient()
	ctx := context.Background()

	tx := pay(1, 2, 100, 5, 0)
	res, err := c.SubmitTx(ctx, addr, tx)
	if err != nil || res.TxID != core.TxID(tx) || !res.HasReport {
		t.Fatalf("submit: %+v, %v", res, err)
	}
	if p := n.Mempool(); len(p) != 1 || p[0] != tx {
		t.Fatalf("mempool: %v", p)
	}

	// Rejections: status 400, a specific code, the sentinel through the
	// client, and a load report on every answer.
	for _, c2 := range []struct {
		name     string
		tx       core.Tx
		code     string
		sentinel error
	}{
		{"duplicate", tx, CodeDuplicate, mempool.ErrDuplicate},
		{"sender pending", pay(1, 3, 1, 1, 0), CodeSenderPending, mempool.ErrSenderPending},
		{"balance", pay(2, 3, consensus.DemoBalance, 1, 0), CodeBalance, core.ErrBalance},
		{"seq", pay(2, 3, 1, 1, 7), CodeSeq, core.ErrSeq},
		{"overflow", pay(2, 3, math.MaxUint64, 1, 0), CodeOverflow, core.ErrOverflow},
	} {
		res, err := c.SubmitTx(ctx, addr, c2.tx)
		wantAPIError(t, c2.name, err, http.StatusBadRequest, c2.code, c2.sentinel)
		if !res.HasReport {
			t.Errorf("%s: no load report on a rejection", c2.name)
		}
	}

	// Malformed requests.
	j, _ := json.Marshal(TxToJSON(pay(3, 4, 1, 1, 0)))
	for _, c2 := range []struct{ name, body string }{
		{"empty", ""},
		{"not JSON", "hello"},
		{"array", "[]"},
		{"truncated", string(j[:len(j)-1])},
		{"unknown field", strings.Replace(string(j), `"seq"`, `"nonce":1,"seq"`, 1)},
		{"two values", string(j) + string(j)},
		{"negative amount", strings.Replace(string(j), `"amount":1`, `"amount":-1`, 1)},
		{"amount too large", strings.Replace(string(j), `"amount":1`, `"amount":18446744073709551616`, 1)},
		{"string amount", strings.Replace(string(j), `"amount":1`, `"amount":"1"`, 1)},
		{"bad hex", strings.Replace(string(j), acct(3).String(), "zz", 1)},
		{"oversized", `{"sender":"` + strings.Repeat("0", maxTxBody) + `"}`},
	} {
		st, hdr, data := raw(t, "POST", addr, "/tx", c2.body)
		wantError(t, c2.name, st, data, http.StatusBadRequest, CodeBadRequest)
		if hdr.Get(routing.LoadHeader) == "" {
			t.Errorf("%s: no load header", c2.name)
		}
	}
	// Trailing white space is fine.
	if st, _, data := raw(t, "POST", addr, "/tx", string(j)+"\n "); st != http.StatusOK {
		t.Errorf("trailing space: %d %s", st, data)
	}

	n.Stop()
	_, err = c.SubmitTx(ctx, addr, pay(5, 6, 1, 1, 0))
	wantAPIError(t, "stopped", err, http.StatusServiceUnavailable, CodeUnavailable, nil)
}

// fakeClock is a settable clock for the LoadMeter.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeClock) add(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// gated holds each SubmitTx of a sender with a gate until the gate is
// closed, so the test controls when POST /tx requests finish.
type gated struct {
	*node.Node
	gates   map[core.Hash]chan struct{} // read-only once the server runs
	entered chan core.Hash              // a gated request reached SubmitTx (after the meter's Start)
}

func (g *gated) SubmitTx(tx core.Tx) (core.Hash, error) {
	if ch := g.gates[tx.Sender]; ch != nil {
		g.entered <- tx.Sender
		<-ch
	}
	return g.Node.SubmitTx(tx)
}

// TestLoadMeterOnPostTx scripts POST /tx load against a fake clock and
// checks every reported value against DR-13: n = POST /tx requests in
// flight, s_i = W_i/(a_i+1), ŝ = EWMA(α = 0.2), D = (n+1)·ŝ; GET requests
// are never measured (I4).
func TestLoadMeterOnPostTx(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	g := &gated{Node: newNode(t, nil), gates: map[core.Hash]chan struct{}{}, entered: make(chan core.Hash, 8)}
	for i := range 5 {
		g.gates[acct(i)] = make(chan struct{})
	}
	srv := NewServer(g, routing.NewLoadMeter(clk.now))
	addr := serve(t, srv)
	opened := map[int]bool{}
	release := func(i int) { opened[i] = true; close(g.gates[acct(i)]) }
	t.Cleanup(func() { // runs before the server closes: never leave a handler blocked
		for i := range 5 {
			if !opened[i] {
				close(g.gates[acct(i)])
			}
		}
	})
	c := NewClient()
	ctx := context.Background()

	type answer struct {
		res SubmitResult
		err error
	}
	// send starts a POST /tx and returns once the meter has counted it.
	send := func(sender int) chan answer {
		ch := make(chan answer, 1)
		go func() {
			res, err := c.SubmitTx(ctx, addr, pay(sender, 7, 1, 1, 0))
			ch <- answer{res, err}
		}()
		if got := <-g.entered; got != acct(sender) {
			t.Fatalf("entered %s, want sender %d", got, sender)
		}
		return ch
	}
	tipReport := func() string {
		_, hdr, _ := raw(t, "GET", addr, "/tip", "")
		return hdr.Get(routing.LoadHeader)
	}
	waitReport := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for tipReport() != want {
			if time.Now().After(deadline) {
				t.Fatalf("GET /tip report %q, want %q", tipReport(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	finish := func(ch chan answer, want string) {
		t.Helper()
		a := <-ch
		if a.err != nil {
			t.Fatal(a.err)
		}
		if got := a.res.Report.String(); !a.res.HasReport || got != want {
			t.Fatalf("POST /tx report %q, want %q", got, want)
		}
	}

	// Warm-up: one request alone taking 4 ms. a = 0, s = 4 ms = ŝ; its own
	// answer is sent after its sample, with n = 0: D = 1·4 ms.
	if r := tipReport(); r != "" {
		t.Fatalf("report before the first sample: %q", r)
	}
	a0 := send(0)
	clk.add(4 * time.Millisecond)
	release(0)
	finish(a0, "d=4000,n=0")
	waitReport("d=4000,n=0")

	// Three requests arrive one after another at the same instant, so
	// a = 0, 1, 2. GET /tip sees n immediately: D = (n+1)·4 ms.
	b := send(1)
	waitReport("d=8000,n=1")
	cc := send(2)
	waitReport("d=12000,n=2")
	d := send(3)
	waitReport("d=16000,n=3")
	// Other request classes are not measured: n stays 3.
	raw(t, "GET", addr, "/headers?from=0", "")
	raw(t, "GET", addr, "/blocks?from=0", "")
	waitReport("d=16000,n=3")

	// FIFO service at 4 ms each: W = 4, 8, 12 ms, s = W/(a+1) = 4 ms every
	// time, so ŝ stays 4 ms and only n falls.
	clk.add(4 * time.Millisecond)
	release(1)
	finish(b, "d=12000,n=2")
	clk.add(4 * time.Millisecond)
	release(2)
	finish(cc, "d=8000,n=1")
	clk.add(4 * time.Millisecond)
	release(3)
	finish(d, "d=4000,n=0")

	// A slower request alone: s = 14 ms, ŝ = 4 + 0.2·(14 − 4) = 6 ms.
	e := send(4)
	waitReport("d=8000,n=1")
	clk.add(14 * time.Millisecond)
	release(4)
	finish(e, "d=6000,n=0")

	// A rejected request is a POST /tx too: it is measured (W = 0 on the
	// stopped clock: ŝ = 6 + 0.2·(0 − 6) = 4.8 ms) and carries the header.
	st, hdr, data := raw(t, "POST", addr, "/tx", "{")
	wantError(t, "malformed", st, data, http.StatusBadRequest, CodeBadRequest)
	if got := hdr.Get(routing.LoadHeader); got != "d=4800,n=0" {
		t.Fatalf("report on a 400: %q", got)
	}
	// GETs change nothing.
	for range 20 {
		raw(t, "GET", addr, "/tip", "")
		raw(t, "GET", addr, "/proof/"+core.Hash{}.String(), "")
	}
	waitReport("d=4800,n=0")
}

func TestHeadersAndBlocksFrom(t *testing.T) {
	n := newNode(t, nil)
	grow(t, n, 12, 0)
	addr := serve(t, NewServer(n, nil))
	c := NewClient()
	ctx := context.Background()
	p := testParams()

	for _, from := range []uint64{0, 1, 5, 12, 13, 1000} {
		hs, err := c.Headers(ctx, addr, from)
		if err != nil {
			t.Fatal(err)
		}
		want := n.Headers(from)
		if len(hs) != len(want) {
			t.Fatalf("headers from %d: %d, want %d", from, len(hs), len(want))
		}
		for i := range hs {
			if hs[i] != want[i] {
				t.Fatalf("header %d differs", from+uint64(i))
			}
		}
		bs, err := c.Blocks(ctx, addr, from)
		if err != nil || len(bs) != len(want) {
			t.Fatalf("blocks from %d: %d, %v", from, len(bs), err)
		}
		wantB := n.Blocks(from)
		for i := range bs {
			if bs[i].Header != wantB[i].Header || len(bs[i].Txs) != len(wantB[i].Txs) {
				t.Fatalf("block %d differs", from+uint64(i))
			}
			// The root is recomputed from the decoded transactions (genesis
			// has none and a zero root).
			if from+uint64(i) > 0 && core.MerkleRoot(core.TxIDs(bs[i].Txs)) != bs[i].Header.MerkleRoot {
				t.Fatalf("block %d: transactions do not match the root", from+uint64(i))
			}
		}
	}
	// The decoded chain passes D2's header-chain check from genesis.
	hs, _ := c.Headers(ctx, addr, 0)
	if err := consensus.CheckHeaderChain(p, hs); err != nil {
		t.Fatal(err)
	}
	// Above the tip: an empty list, never null.
	for _, path := range []string{"/headers?from=13", "/blocks?from=13"} {
		st, _, data := raw(t, "GET", addr, path, "")
		if st != http.StatusOK || !(bytes.Contains(data, []byte(`"headers":[]`)) || bytes.Contains(data, []byte(`"blocks":[]`))) {
			t.Errorf("%s: %d %s", path, st, data)
		}
	}

	z := strings.Repeat("0", 64)
	for _, base := range []string{"/headers", "/blocks"} {
		for _, q := range []string{"", "?", "?from=", "?from=-1", "?from=abc", "?from=1.0", "?from=18446744073709551616",
			"?from=1&from=2", "?from=0&locator=0:" + z, "?foo=1"} {
			st, _, data := raw(t, "GET", addr, base+q, "")
			wantError(t, base+q, st, data, http.StatusBadRequest, CodeBadRequest)
		}
		st, _, data := raw(t, "GET", addr, base+"?locator=0:"+z+"&locator=0:"+z, "")
		wantError(t, base+" two locators", st, data, http.StatusBadRequest, CodeBadLocator)
	}
}

// TestLocatorEndpoints: GET /headers?locator= and /blocks?locator= return
// D2's fork point (consensus.ForkPoint inside node.HeadersAfter/BlocksAfter)
// and only what follows it, in one request (DR-14).
func TestLocatorEndpoints(t *testing.T) {
	s := newNode(t, nil)
	sb := grow(t, s, 25, 0)
	addr := serve(t, NewServer(s, nil))
	c := NewClient()
	ctx := context.Background()
	p := testParams()

	check := func(name string, req *node.Node, wantFork uint64) {
		t.Helper()
		loc := req.Locator()
		fork, hs, err := c.HeadersAfter(ctx, addr, loc)
		if err != nil || fork != wantFork {
			t.Fatalf("%s: fork %d, %v; want %d", name, fork, err, wantFork)
		}
		want := s.Headers(fork + 1)
		if len(hs) != len(want) {
			t.Fatalf("%s: %d headers, want %d", name, len(hs), len(want))
		}
		for i := range hs {
			if hs[i] != want[i] {
				t.Fatalf("%s: header %d differs", name, i)
			}
		}
		// The light client's check: anchored on its own header at the fork.
		if len(hs) > 0 {
			own := req.Headers(fork)[0]
			if err := consensus.CheckHeaderChain(p, append([]core.Header{own}, hs...)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		bfork, bs, err := c.BlocksAfter(ctx, addr, loc)
		if err != nil || bfork != fork || len(bs) != len(want) {
			t.Fatalf("%s: blocks fork %d, %d blocks, %v", name, bfork, len(bs), err)
		}
		for i := range bs {
			if bs[i].Header != want[i] || core.MerkleRoot(core.TxIDs(bs[i].Txs)) != want[i].MerkleRoot {
				t.Fatalf("%s: block %d differs", name, i)
			}
		}
	}

	check("same chain", s, 25)

	// A requester holding the first 10 blocks: fork 10, 15 headers.
	pre := newNode(t, nil)
	for _, b := range sb[:10] {
		if err := pre.ReceiveBlock(b, "test:1"); err != nil {
			t.Fatal(err)
		}
	}
	check("prefix", pre, 10)

	// A requester on its own branch after block 10 (5 blocks of another
	// sender): the fork point is the highest of its locator heights at or
	// below 10, computed independently with D2's ForkPoint.
	fk := newNode(t, nil)
	for _, b := range sb[:10] {
		fk.ReceiveBlock(b, "test:1")
	}
	grow(t, fk, 5, 1)
	sHashes := s.Headers(0)
	want, err := consensus.ForkPoint(fk.Locator(), 25, func(h uint64) core.Hash { return core.HeaderHash(sHashes[h]) })
	if err != nil || want > 10 {
		t.Fatalf("expected fork %d, %v", want, err)
	}
	check("branch", fk, want)

	// Only genesis in common.
	g := newNode(t, nil)
	grow(t, g, 3, 2)
	check("genesis only", g, 0)

	// Malformed locators: 400 bad_locator, matching consensus.ErrBadLocator.
	z := core.Hash{}.String()
	gen := core.HeaderHash(s.Headers(0)[0]).String()
	tooMany := make([]consensus.LocatorEntry, consensus.MaxLocatorEntries+1)
	for i := range tooMany {
		tooMany[i].Height = uint64(len(tooMany) - 1 - i)
	}
	// Syntactically broken: sent verbatim (the client cannot build them).
	for _, q := range []string{"", "5", "5:" + z[:10], "x:" + gen, "5:" + z + ",", FormatLocator(tooMany)} {
		for _, base := range []string{"/headers", "/blocks"} {
			st, _, data := raw(t, "GET", addr, base+"?locator="+q, "")
			wantError(t, base+" locator "+q, st, data, http.StatusBadRequest, CodeBadLocator)
		}
	}
	// Well-formed but invalid (D2's ForkPoint rejects them): through the client.
	for _, q := range []string{"3:" + z + ",3:" + z + ",0:" + gen, "1:" + z + ",2:" + z + ",0:" + gen, "5:" + z + ",1:" + z} {
		loc, err := ParseLocator(q)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = c.HeadersAfter(ctx, addr, loc)
		wantAPIError(t, "headers locator "+q, err, http.StatusBadRequest, CodeBadLocator, consensus.ErrBadLocator)
		_, _, err = c.BlocksAfter(ctx, addr, loc)
		wantAPIError(t, "blocks locator "+q, err, http.StatusBadRequest, CodeBadLocator, consensus.ErrBadLocator)
	}

	// Another genesis: 409 genesis_mismatch.
	other := []consensus.LocatorEntry{{Height: 2, Hash: core.Hash{1}}, {Height: 0, Hash: core.Hash{2}}}
	_, _, err = c.HeadersAfter(ctx, addr, other)
	wantAPIError(t, "headers genesis", err, http.StatusConflict, CodeGenesisMismatch, consensus.ErrGenesisMismatch)
	_, _, err = c.BlocksAfter(ctx, addr, other)
	wantAPIError(t, "blocks genesis", err, http.StatusConflict, CodeGenesisMismatch, consensus.ErrGenesisMismatch)

	// After Stop: 503.
	s.Stop()
	_, _, err = c.HeadersAfter(ctx, addr, pre.Locator())
	wantAPIError(t, "stopped", err, http.StatusServiceUnavailable, CodeUnavailable, nil)
}

func TestProofEndpoint(t *testing.T) {
	n := newNode(t, nil)
	grow(t, n, 1, 0)
	_, tip := n.Tip()
	var txs []core.Tx
	for i := 1; i <= 5; i++ {
		txs = append(txs, pay(i, 7, uint64(i), uint64(10-i), 0))
	}
	if err := n.ReceiveBlock(buildBlock(t, tip, txs), "test:1"); err != nil {
		t.Fatal(err)
	}
	addr := serve(t, NewServer(n, nil))
	c := NewClient()
	ctx := context.Background()
	root := n.Headers(2)[0].MerkleRoot

	for _, tx := range txs {
		id := core.TxID(tx)
		res, err := c.Proof(ctx, addr, id)
		if err != nil || res.Height != 2 || res.Tx != tx || len(res.Proof) != 3 {
			t.Fatalf("proof: %+v, %v", res, err)
		}
		if !core.MerkleVerify(root, core.TxID(res.Tx), res.Proof) {
			t.Fatal("proof does not verify against the node's header")
		}
		// Upper-case hex in the path works too.
		if st, _, data := raw(t, "GET", addr, "/proof/"+strings.ToUpper(id.String()), ""); st != http.StatusOK {
			t.Fatalf("upper-case txid: %d %s", st, data)
		}
	}
	// No root in the answer (M-R8): exactly height, tx, proof.
	_, _, data := raw(t, "GET", addr, "/proof/"+core.TxID(txs[0]).String(), "")
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil || len(m) != 3 || m["height"] == nil || m["tx"] == nil || m["proof"] == nil {
		t.Errorf("proof JSON: %s", data)
	}

	_, err := c.Proof(ctx, addr, core.Hash{9})
	wantAPIError(t, "unknown txid", err, http.StatusNotFound, CodeNotFound, core.ErrNotFound)
	// A pending transaction has no proof yet.
	pending := pay(6, 7, 1, 1, 0)
	if _, err := c.SubmitTx(ctx, addr, pending); err != nil {
		t.Fatal(err)
	}
	_, err = c.Proof(ctx, addr, core.TxID(pending))
	wantAPIError(t, "pending txid", err, http.StatusNotFound, CodeNotFound, core.ErrNotFound)
	for _, bad := range []string{"zz", strings.Repeat("0", 63), strings.Repeat("0", 65), strings.Repeat("x", 64)} {
		st, _, data := raw(t, "GET", addr, "/proof/"+bad, "")
		wantError(t, "proof "+bad, st, data, http.StatusBadRequest, CodeBadRequest)
	}
	n.Stop()
	_, err = c.Proof(ctx, addr, core.TxID(txs[0]))
	wantAPIError(t, "stopped", err, http.StatusServiceUnavailable, CodeUnavailable, nil)
}

func TestPostBlock(t *testing.T) {
	n := newNode(t, nil)
	grow(t, n, 2, 0)
	addr := serve(t, NewServer(n, nil))
	c := NewClient()
	ctx := context.Background()
	const from = "127.0.0.1:1" // nothing listens there

	_, tip := n.Tip()
	tx := pay(1, 2, 10, 1, 0)
	b := buildBlock(t, tip, []core.Tx{tx})
	if r, err := c.PostBlock(ctx, addr, b, from); err != nil || r != ResultAccepted {
		t.Fatalf("on tip: %q, %v", r, err)
	}
	if h, hd := n.Tip(); h != 3 || hd != b.Header {
		t.Fatal("block not appended")
	}
	if r, err := c.PostBlock(ctx, addr, b, from); err != nil || r != ResultStale {
		t.Fatalf("duplicate: %q, %v", r, err)
	}
	// An older block (parent just below the tip): stale, no sync attempted.
	if r, err := c.PostBlock(ctx, addr, n.Blocks(3)[0], from); err != nil || r != ResultStale {
		t.Fatalf("old block: %q, %v", r, err)
	}

	// Invalid blocks on the tip: 400 invalid_block, chain unchanged.
	_, tip = n.Tip()
	good := buildBlock(t, tip, []core.Tx{pay(1, 2, 10, 1, 1)})
	badWork := good
	for badWork.Header.Nonce = good.Header.Nonce + 1; core.PowCheck(core.HeaderHash(badWork.Header), testDifficulty); badWork.Header.Nonce++ {
	}
	badMerkle := core.Block{Header: good.Header, Txs: []core.Tx{pay(1, 2, 11, 1, 1)}}
	badDiff := good
	badDiff.Header.Difficulty = testDifficulty + 1
	overspend := buildBlock(t, tip, []core.Tx{pay(1, 2, consensus.DemoBalance, 1, 1)})
	empty := buildBlock(t, tip, nil)
	for name, bad := range map[string]core.Block{"work": badWork, "merkle": badMerkle, "difficulty": badDiff, "balance": overspend, "empty": empty} {
		_, err := c.PostBlock(ctx, addr, bad, from)
		wantAPIError(t, "invalid "+name, err, http.StatusBadRequest, CodeInvalidBlock, nil)
		if h, _ := n.Tip(); h != 3 {
			t.Fatalf("%s changed the chain", name)
		}
	}

	// A block that does not fit: the node tries to sync from "from", which
	// does not answer: 502 sync_failed.
	far := buildBlock(t, core.Header{Nonce: 42}, []core.Tx{pay(3, 4, 1, 1, 0)})
	_, err := c.PostBlock(ctx, addr, far, from)
	wantAPIError(t, "unreachable sender", err, http.StatusBadGateway, CodeSyncFailed, nil)

	// Malformed requests.
	bj, _ := json.Marshal(BlockRequest{Block: BlockToJSON(good), From: from})
	for _, c2 := range []struct{ name, body string }{
		{"empty", ""},
		{"not JSON", "{"},
		{"unknown field", strings.Replace(string(bj), `"from"`, `"x":1,"from"`, 1)},
		{"no from", strings.Replace(string(bj), `"from":"`+from+`"`, `"from":""`, 1)},
		{"from without port", strings.Replace(string(bj), from, "127.0.0.1", 1)},
		{"bad hex", strings.Replace(string(bj), good.Header.MerkleRoot.String(), "zz", 1)},
		{"two values", string(bj) + "{}"},
		{"oversized", `{"from":"` + strings.Repeat("a", maxBlockBody) + `"}`},
	} {
		st, _, data := raw(t, "POST", addr, "/block", c2.body)
		wantError(t, c2.name, st, data, http.StatusBadRequest, CodeBadRequest)
	}

	n.Stop()
	_, err = c.PostBlock(ctx, addr, good, from)
	wantAPIError(t, "stopped", err, http.StatusServiceUnavailable, CodeUnavailable, nil)
}

func TestRoutingErrors(t *testing.T) {
	addr := serve(t, NewServer(newNode(t, nil), nil))
	for _, c := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/nothing", http.StatusNotFound},
		{"GET", "/proof/", http.StatusNotFound},
		{"GET", "/tx", http.StatusMethodNotAllowed},
		{"POST", "/tip", http.StatusMethodNotAllowed},
		{"DELETE", "/block", http.StatusMethodNotAllowed},
		{"PUT", "/headers?from=0", http.StatusMethodNotAllowed},
	} {
		if st, _, _ := raw(t, c.method, addr, c.path, ""); st != c.status {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, st, c.status)
		}
	}
	// The client reports a non-JSON error answer with its status.
	_, err := NewClient().do(context.Background(), "GET", addr, "/nothing", nil, &struct{}{})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusNotFound || ae.Code != "" || ae.Message == "" {
		t.Errorf("non-JSON error: %v", err)
	}
}

// TestClientRejectsBadAnswers: answers are checked after decoding, and
// hashes are recomputed locally rather than trusted.
func TestClientRejectsBadAnswers(t *testing.T) {
	tx := pay(1, 2, 3, 4, 0)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tx", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(routing.LoadHeader, "d=5,n=1")
		writeJSON(w, 200, SubmitResponse{TxID: core.Hash{1}.String()}) // wrong txid
	})
	mux.HandleFunc("GET /tip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(routing.LoadHeader, "garbage")
		io.WriteString(w, `{"height":1,"header":{"prev":"zz"}}`)
	})
	mux.HandleFunc("GET /headers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, HeadersFromResponse{From: 7, Headers: HeadersToJSON(nil)})
	})
	mux.HandleFunc("GET /blocks", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `not json`)
	})
	mux.HandleFunc("GET /proof/{txid}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, ProofResponse{Tx: TxToJSON(tx), Proof: []ProofStepJSON{{Sibling: core.Hash{}.String(), Side: "middle"}}})
	})
	mux.HandleFunc("POST /block", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, BlockResponse{Result: "maybe"})
	})
	addr := serve(t, mux)
	c := NewClient()
	ctx := context.Background()

	res, err := c.SubmitTx(ctx, addr, tx)
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("wrong txid: %v", err)
	}
	if !res.HasReport || res.Report.InFlight != 1 || res.Report.Delay != 5*time.Microsecond {
		t.Errorf("report kept with a bad answer: %+v", res)
	}
	if _, err := c.Tip(ctx, addr); !errors.Is(err, ErrMalformed) {
		t.Errorf("bad header: %v", err)
	}
	if _, err := c.Headers(ctx, addr, 3); !errors.Is(err, ErrMalformed) {
		t.Errorf("wrong from: %v", err)
	}
	if _, err := c.Blocks(ctx, addr, 0); !errors.Is(err, ErrMalformed) {
		t.Errorf("not JSON: %v", err)
	}
	if _, err := c.Proof(ctx, addr, core.TxID(tx)); !errors.Is(err, ErrMalformed) {
		t.Errorf("bad side: %v", err)
	}
	if _, err := c.PostBlock(ctx, addr, core.Block{}, "a:1"); !errors.Is(err, ErrMalformed) {
		t.Errorf("bad result: %v", err)
	}
	// A malformed load header is no report, not an error.
	mux2 := http.NewServeMux()
	mux2.HandleFunc("GET /tip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(routing.LoadHeader, "d=1,n=-1")
		writeJSON(w, 200, TipResponse{Header: HeaderToJSON(core.Header{})})
	})
	info, err := c.Tip(ctx, serve(t, mux2))
	if err != nil || info.HasReport {
		t.Errorf("malformed load header: %+v, %v", info, err)
	}
}

func TestProbe(t *testing.T) {
	n := newNode(t, nil)
	addr := serve(t, NewServer(n, nil))
	c := NewClient()
	ctx := context.Background()

	res, err := c.Probe(ctx, addr)
	if err != nil || res.RTT <= 0 || res.RTT > routing.ProbeTimeout || res.HasReport {
		t.Fatalf("probe before any POST /tx: %+v, %v", res, err)
	}
	sub, err := c.SubmitTx(ctx, addr, pay(0, 1, 1, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	res, err = c.Probe(ctx, addr)
	if err != nil || !res.HasReport || res.Report != sub.Report || res.Report.InFlight != 0 {
		t.Fatalf("probe: %+v (submit report %+v), %v", res, sub.Report, err)
	}
	// The method value is a routing.ProbeFunc.
	var _ routing.ProbeFunc = c.Probe

	// A refused connection fails fast, without an APIError.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	start := time.Now()
	_, err = c.Probe(ctx, dead)
	var ae *APIError
	if err == nil || errors.As(err, &ae) || time.Since(start) > routing.ProbeTimeout {
		t.Errorf("refused: %v after %v", err, time.Since(start))
	}
}

// hanging returns a server that never answers until the test ends.
func hanging(t *testing.T) string {
	t.Helper()
	stop := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(stop); ts.Close() })
	return strings.TrimPrefix(ts.URL, "http://")
}

// TestFixedTimeouts: a node that accepts the connection but never answers
// fails a request after RequestTimeout and a probe after ProbeTimeout, as a
// timeout error; the selector fails over past it and marks it down.
func TestFixedTimeouts(t *testing.T) {
	c := NewClient()
	ctx := context.Background()

	t.Run("request", func(t *testing.T) {
		t.Parallel()
		hang := hanging(t)
		start := time.Now()
		_, err := c.Tip(ctx, hang)
		el := time.Since(start)
		if !IsTimeout(err) || el < routing.RequestTimeout-50*time.Millisecond || el > routing.RequestTimeout+time.Second {
			t.Errorf("request: %v after %v", err, el)
		}
	})

	t.Run("stalled body", func(t *testing.T) {
		t.Parallel()
		// Headers arrive at once, the body never: only the whole-request
		// limit (not the header timeout) ends this.
		stop := make(chan struct{})
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"height":`)
			w.(http.Flusher).Flush()
			select {
			case <-stop:
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(func() { close(stop); ts.Close() })
		start := time.Now()
		_, err := c.Tip(ctx, strings.TrimPrefix(ts.URL, "http://"))
		el := time.Since(start)
		if !IsTimeout(err) || el < routing.RequestTimeout-50*time.Millisecond || el > routing.RequestTimeout+time.Second {
			t.Errorf("stalled body: %v after %v", err, el)
		}
	})

	t.Run("probe", func(t *testing.T) {
		t.Parallel()
		hang := hanging(t)
		start := time.Now()
		_, err := c.Probe(ctx, hang)
		el := time.Since(start)
		if !IsTimeout(err) || el < routing.ProbeTimeout-50*time.Millisecond || el > routing.ProbeTimeout+500*time.Millisecond {
			t.Errorf("probe: %v after %v", err, el)
		}
	})

	t.Run("connect", func(t *testing.T) {
		t.Parallel()
		// 10.255.255.1 is normally unroutable: the SYN gets no answer and
		// the dial times out at ConnectTimeout. Where the network refuses it
		// at once instead, the request must still fail within that bound.
		start := time.Now()
		_, err := c.Tip(ctx, "10.255.255.1:80")
		el := time.Since(start)
		if err == nil || el > routing.ConnectTimeout+time.Second {
			t.Errorf("connect: %v after %v", err, el)
		}
		if IsTimeout(err) && el < routing.ConnectTimeout-50*time.Millisecond {
			t.Errorf("connect timed out early: %v", el)
		}
		t.Logf("connect: %v after %v", err, el)
	})

	t.Run("failover", func(t *testing.T) {
		t.Parallel()
		hang := hanging(t)
		good := serve(t, NewServer(newNode(t, nil), nil))
		sel := routing.NewSelector(routing.New(150, hang, good), routing.Options{Strategy: routing.StrategyRing})
		// A key whose ring owner is the hanging node.
		key := ""
		for i := 0; ; i++ {
			key = fmt.Sprint("client-", i)
			if p, _ := sel.Pick(key); p == hang {
				break
			}
		}
		start := time.Now()
		var tried []string
		used, err := sel.Do(ctx, key, func(ctx context.Context, nd string) error {
			tried = append(tried, nd)
			sel.RecordSend(nd)
			res, err := c.SubmitTx(ctx, nd, pay(0, 1, 1, 1, 0))
			if res.HasReport {
				sel.RecordReport(nd, res.Report)
			}
			return err
		})
		el := time.Since(start)
		if err != nil || used != good || len(tried) != 2 || tried[0] != hang {
			t.Fatalf("Do: %s, %v, tried %v", used, err, tried)
		}
		if el < routing.RequestTimeout-50*time.Millisecond || el > routing.RequestTimeout+time.Second {
			t.Errorf("failover after %v", el)
		}
	})

	t.Run("probe health", func(t *testing.T) {
		t.Parallel()
		hang := hanging(t)
		good := serve(t, NewServer(newNode(t, nil), nil))
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		dead := ln.Addr().String()
		ln.Close()
		sel := routing.NewSelector(routing.New(150, hang, good, dead), routing.Options{})
		for range routing.FailuresToDown {
			sel.ProbeOnce(ctx, c.Probe)
		}
		// hang and dead are down after two failed probe rounds: every key
		// goes to the only healthy node.
		for i := range 100 {
			if p, err := sel.Pick(fmt.Sprint("k", i)); err != nil || p != good {
				t.Fatalf("Pick: %s, %v", p, err)
			}
		}
	})
}

// newPeer starts a full node with an HTTP server and a PeerTransport to
// peers. The listener exists before the node, so the node knows its address.
func newPeer(t *testing.T, peers ...string) (*node.Node, string, *PeerTransport) {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	self := ts.Listener.Addr().String()
	pt := NewPeerTransport(NewClient(), self, peers, t.Logf)
	n := newNode(t, pt)
	ts.Config.Handler = NewServer(n, nil)
	ts.Start()
	t.Cleanup(func() { n.Stop(); pt.Wait(); ts.Close() })
	return n, self, pt
}

func waitTip(t *testing.T, want core.Header, ns ...*node.Node) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for _, n := range ns {
		for {
			if _, h := n.Tip(); h == want {
				break
			}
			if time.Now().After(deadline) {
				h, _ := n.Tip()
				t.Fatalf("node at height %d did not reach the tip", h)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestPeerRelay runs nodes over real HTTP: relay of mined blocks, catch-up of
// a node that missed blocks (locator sync through PeerTransport.FetchBlocks),
// and fork resolution between nodes that mined apart.
func TestPeerRelay(t *testing.T) {
	ctx := context.Background()
	c := NewClient()

	// a, b, c3 fully connected; a mines from its mempool.
	addrs := make([]string, 3)
	lns := make([]net.Listener, 3)
	for i := range lns {
		lns[i], _ = net.Listen("tcp", "127.0.0.1:0")
		addrs[i] = lns[i].Addr().String()
	}
	var nodes []*node.Node
	var pts []*PeerTransport
	for i := range 3 {
		pt := NewPeerTransport(c, addrs[i], addrs, t.Logf) // self is skipped
		if len(pt.Peers()) != 2 {
			t.Fatalf("peers: %v", pt.Peers())
		}
		n := newNode(t, pt)
		ts := &httptest.Server{Listener: lns[i], Config: &http.Server{Handler: NewServer(n, nil)}}
		ts.Start()
		t.Cleanup(func() { n.Stop(); pt.Wait(); ts.Close() })
		nodes, pts = append(nodes, n), append(pts, pt)
	}
	nodes[0].Start(ctx)
	for i := range 3 {
		if _, err := c.SubmitTx(ctx, addrs[0], pay(i, 7, 5, 1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for nodes[0].MempoolSize() > 0 || func() bool { h, _ := nodes[0].Tip(); return h == 0 }() {
		if time.Now().After(deadline) {
			t.Fatal("a did not mine")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h, tipA := nodes[0].Tip() // before Stop, after which Tip reads as zero; the mempool is empty, so a mines no more
	nodes[0].Stop()
	pts[0].Wait()
	waitTip(t, tipA, nodes[1], nodes[2])
	for i := range 3 {
		if _, err := c.Proof(ctx, addrs[1], core.TxID(pay(i, 7, 5, 1, 0))); err != nil {
			t.Fatalf("b has no proof for tx %d: %v", i, err)
		}
	}

	// A late node peered with b. b's peer list was fixed before it existed,
	// so the test relays b's next block to it with the client, from b's
	// address: the late node lacks the parent and fetches b's whole chain
	// in one locator request. pt1 is a transport acting as b.
	late, lateAddr, _ := newPeer(t, addrs[1])
	pt1 := NewPeerTransport(c, addrs[1], []string{lateAddr}, t.Logf)
	fork, bs, err := pt1.FetchBlocks(addrs[1], late.Locator())
	if err != nil || fork != 0 || len(bs) != int(h) {
		t.Fatalf("FetchBlocks: fork %d, %d blocks, %v", fork, len(bs), err)
	}
	next := buildBlock(t, tipA, []core.Tx{pay(3, 7, 1, 1, 0)})
	if err := nodes[1].ReceiveBlock(next, "test:1"); err != nil {
		t.Fatal(err)
	}
	if r, err := c.PostBlock(ctx, lateAddr, next, addrs[1]); err != nil || r != ResultAccepted {
		t.Fatalf("late node: %q, %v", r, err)
	}
	if lh, lt := late.Tip(); lh != h+1 || lt != next.Header {
		t.Fatalf("late node at %d", lh)
	}

	// Fork: x and y share nothing but genesis; y is longer. y's tip posted
	// to x makes x fetch y's chain and switch.
	x, xAddr, _ := newPeer(t)
	y, yAddr, _ := newPeer(t)
	grow(t, x, 3, 4)
	grow(t, y, 5, 5)
	_, ytip := y.Tip()
	if r, err := c.PostBlock(ctx, xAddr, y.Blocks(5)[0], yAddr); err != nil || r != ResultAccepted {
		t.Fatalf("fork: %q, %v", r, err)
	}
	if _, xt := x.Tip(); xt != ytip {
		t.Fatal("x did not switch to y's chain")
	}
	// The other way the chain is not longer: stale.
	if r, err := c.PostBlock(ctx, yAddr, x.Blocks(5)[0], xAddr); err != nil || r != ResultStale {
		t.Fatalf("reverse: %q, %v", r, err)
	}
	// A block whose parent x lacks makes x sync from the sender; y has
	// exactly x's chain, so nothing follows the fork point: stale.
	stray := buildBlock(t, core.Header{Nonce: 7}, []core.Tx{pay(6, 7, 1, 1, 0)})
	if r, err := c.PostBlock(ctx, xAddr, stray, yAddr); err != nil || r != ResultStale {
		t.Fatalf("stray block: %q, %v", r, err)
	}
	if _, xt := x.Tip(); xt != ytip {
		t.Fatal("x changed after a stray block")
	}

	// A sender serving an invalid chain (bad proof of work): x replays it,
	// rejects it (node.ErrPeerChain) and answers 400 invalid_block.
	genesis := x.Headers(0)[0]
	bad := buildBlock(t, genesis, []core.Tx{pay(6, 7, 1, 1, 0)})
	for bad.Header.Nonce++; core.PowCheck(core.HeaderHash(bad.Header), testDifficulty); bad.Header.Nonce++ {
	}
	liar := http.NewServeMux()
	liar.HandleFunc("GET /blocks", func(w http.ResponseWriter, r *http.Request) {
		chain := []core.Block{bad}
		for range 9 { // longer than x's 5 blocks; only the first is checked before rejection
			chain = append(chain, bad)
		}
		writeJSON(w, 200, BlocksAfterResponse{Fork: 0, Blocks: BlocksToJSON(chain)})
	})
	liarAddr := serve(t, liar)
	_, err = c.PostBlock(ctx, xAddr, stray, liarAddr)
	wantAPIError(t, "invalid peer chain", err, http.StatusBadRequest, CodeInvalidBlock, nil)
	if _, xt := x.Tip(); xt != ytip {
		t.Fatal("x changed after an invalid peer chain")
	}

	// FetchBlocks errors wrap ErrFetch: unreachable peer, other genesis.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	if _, _, err := pt1.FetchBlocks(dead, late.Locator()); !errors.Is(err, ErrFetch) {
		t.Errorf("dead peer: %v", err)
	}
	other := []consensus.LocatorEntry{{Height: 0, Hash: core.Hash{1}}}
	if _, _, err := pt1.FetchBlocks(addrs[1], other); !errors.Is(err, ErrFetch) || !errors.Is(err, consensus.ErrGenesisMismatch) {
		t.Errorf("other genesis: %v", err)
	}
}

// TestConcurrentClients: many goroutines submit and read at once (-race);
// afterwards no POST /tx is counted in flight.
func TestConcurrentClients(t *testing.T) {
	n := newNode(t, nil)
	grow(t, n, 3, 0)
	srv := NewServer(n, nil)
	addr := serve(t, srv)
	c := NewClient()
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				tx := pay(g, 7, 1, 1, uint64(i)) // only seq 0 is admissible; the rest is rejected
				c.SubmitTx(ctx, addr, tx)
				c.Tip(ctx, addr)
				c.HeadersAfter(ctx, addr, n.Locator())
				c.Proof(ctx, addr, core.TxID(tx))
			}
		}()
	}
	wg.Wait()
	rep, ok := srv.Meter().Report()
	if !ok || rep.InFlight != 0 {
		t.Fatalf("after the load: %+v %v", rep, ok)
	}
	if n.MempoolSize() != 8 {
		t.Fatalf("mempool %d, want 8", n.MempoolSize())
	}
}
