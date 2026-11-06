package consensus

import (
	"errors"
	"math"
	"math/bits"
	"strconv"
	"strings"
	"testing"

	"github.com/Anon-HP/Aletheia/internal/core"
)

// testParams uses a low difficulty so that tests mine in milliseconds.
func testParams() Params {
	p := Default()
	p.Difficulty = 10
	p.Genesis.Difficulty = 10
	return p
}

// lzb counts leading zero bits in Go, independently of the C++ predicate.
func lzb(h core.Hash) int {
	n := 0
	for _, b := range h {
		if b != 0 {
			return n + bits.LeadingZeros8(b)
		}
		n += 8
	}
	return n
}

// mine solves h (prev, root and timestamp already set) by a sequential
// search; the miner package is not used here.
func mine(t *testing.T, h core.Header) core.Header {
	t.Helper()
	for start := uint64(0); ; start += 1 << 16 {
		n, _, found, err := core.PowSearch(h, start, 1<<16, h.Difficulty)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			h.Nonce = n
			return h
		}
	}
}

// child returns a mined successor of prev at difficulty d.
func child(t *testing.T, prev core.Header, d uint32, ts int64) core.Header {
	t.Helper()
	return mine(t, core.Header{
		Version:    1,
		Prev:       core.HeaderHash(prev),
		MerkleRoot: core.SHA256([]byte{byte(ts)}),
		Timestamp:  ts,
		Difficulty: d,
	})
}

// chain returns genesis followed by n mined headers.
func chain(t *testing.T, p Params, n int) []core.Header {
	t.Helper()
	hs := []core.Header{p.Genesis}
	for i := 1; i <= n; i++ {
		hs = append(hs, child(t, hs[i-1], p.Difficulty, p.Genesis.Timestamp+int64(i)))
	}
	return hs
}

func TestDefault(t *testing.T) {
	p := Default()
	if p.Difficulty != DefaultDifficulty || p.Genesis.Difficulty != p.Difficulty {
		t.Fatal("difficulty")
	}
	if p.Difficulty == 0 || p.Difficulty > core.MaxDifficulty {
		t.Fatalf("difficulty %d out of range", p.Difficulty)
	}
	if p.MaxTxPerBlock <= 0 {
		t.Fatal("MaxTxPerBlock must be positive")
	}
	if p.Genesis.Prev != (core.Hash{}) || p.Genesis.MerkleRoot != (core.Hash{}) {
		t.Fatal("genesis must have zero prev and merkle root")
	}
	if len(p.Allocations) != DemoAccounts {
		t.Fatal("allocation count")
	}
	seen := map[core.Hash]bool{}
	for i, a := range p.Allocations {
		if a.Account != DemoAccount(i) || a.Balance != DemoBalance || seen[a.Account] {
			t.Fatalf("allocation %d", i)
		}
		seen[a.Account] = true
	}
	if DemoAccount(0) != core.SHA256([]byte("aletheia/account/0")) {
		t.Fatal("DemoAccount derivation")
	}

	// The ledger accepts the default genesis and allocations.
	l, err := core.NewLedger(p.Genesis, p.Allocations)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if bal, seq, _ := l.Account(DemoAccount(3)); bal != DemoBalance || seq != 0 {
		t.Fatal("genesis balance")
	}

	// Every call returns the same values but independent slices.
	q := Default()
	if q.Genesis != p.Genesis || core.HeaderHash(q.Genesis) != core.HeaderHash(p.Genesis) {
		t.Fatal("Default is not deterministic")
	}
	q.Allocations[0].Balance = 1
	if Default().Allocations[0].Balance != DemoBalance {
		t.Fatal("Allocations slice is shared between calls")
	}
}

func TestCheckHeaderValid(t *testing.T) {
	p := testParams()
	hs := chain(t, p, 5)
	for i := 1; i < len(hs); i++ {
		if err := CheckHeader(p, hs[i-1], hs[i]); err != nil {
			t.Fatalf("header %d: %v", i, err)
		}
	}
}

func TestCheckHeaderLinkage(t *testing.T) {
	p := testParams()
	hs := chain(t, p, 2)
	// Any change to prev's fields changes its hash and breaks the link.
	mods := []func(*core.Header){
		func(h *core.Header) { h.Version++ },
		func(h *core.Header) { h.Prev[31] ^= 1 },
		func(h *core.Header) { h.MerkleRoot[0] ^= 1 },
		func(h *core.Header) { h.Timestamp++ },
		func(h *core.Header) { h.Difficulty++ },
		func(h *core.Header) { h.Nonce++ },
	}
	for i, mod := range mods {
		prev := hs[1]
		mod(&prev)
		if err := CheckHeader(p, prev, hs[2]); err != ErrLinkage {
			t.Fatalf("mod %d: %v, want ErrLinkage", i, err)
		}
	}
	// A header built on genesis is not a successor of block 1.
	if err := CheckHeader(p, hs[1], hs[1]); err != ErrLinkage {
		t.Fatalf("got %v, want ErrLinkage", err)
	}
	// Linkage is checked first: a header that fails everything reports it.
	bad := hs[2]
	bad.Prev = core.Hash{}
	bad.Difficulty = p.Difficulty + 1
	if err := CheckHeader(p, hs[1], bad); err != ErrLinkage {
		t.Fatalf("got %v, want ErrLinkage first", err)
	}
}

// The difficulty field must equal the network difficulty exactly: a header
// genuinely mined at a higher or a lower difficulty is rejected.
func TestCheckHeaderDifficulty(t *testing.T) {
	p := testParams()
	g := p.Genesis
	for _, d := range []uint32{0, p.Difficulty - 1, p.Difficulty + 1, p.Difficulty + 4} {
		h := child(t, g, d, 99)
		if err := CheckHeader(p, g, h); err != ErrDifficulty {
			t.Fatalf("mined at d=%d: %v, want ErrDifficulty", d, err)
		}
	}
	// A difficulty field above 256 is a difficulty failure as well.
	h := child(t, g, p.Difficulty, 7)
	h.Difficulty = core.MaxDifficulty + 1
	if err := CheckHeader(p, g, h); err != ErrDifficulty {
		t.Fatalf("d=257: %v", err)
	}
}

// The work check recomputes the hash: it agrees with an independent count
// for every nonce, and tampering after mining is caught unless the tampered
// header happens to meet the difficulty too.
func TestCheckHeaderWork(t *testing.T) {
	p := testParams()
	g := p.Genesis
	h := core.Header{Version: 1, Prev: core.HeaderHash(g), Timestamp: 5, Difficulty: p.Difficulty}
	met, unmet := 0, 0
	for n := uint64(0); n < 4000; n++ {
		h.Nonce = n
		want := lzb(core.HeaderHash(h)) >= int(p.Difficulty)
		err := CheckHeader(p, g, h)
		switch {
		case want && err == nil:
			met++
		case !want && err == ErrWork:
			unmet++
		default:
			t.Fatalf("nonce %d: err %v, independent check says %v", n, err, want)
		}
	}
	if met == 0 || unmet == 0 {
		t.Fatalf("met %d, unmet %d: both cases must occur", met, unmet)
	}

	solved := child(t, g, p.Difficulty, 11)
	for i := 0; i < 64; i++ {
		tam := solved
		tam.MerkleRoot[i%32] ^= byte(1 << (i % 8))
		want := lzb(core.HeaderHash(tam)) >= int(p.Difficulty)
		if err := CheckHeader(p, g, tam); (err == nil) != want || (err != nil && err != ErrWork) {
			t.Fatalf("tamper %d: %v", i, err)
		}
	}

	// At the default difficulty an unmined header fails on work.
	d := Default()
	u := core.Header{Prev: core.HeaderHash(d.Genesis), Difficulty: d.Difficulty}
	if lzb(core.HeaderHash(u)) < int(d.Difficulty) {
		if err := CheckHeader(d, d.Genesis, u); err != ErrWork {
			t.Fatalf("unmined default header: %v", err)
		}
	}
}

func TestCheckHeaderChain(t *testing.T) {
	p := testParams()
	hs := chain(t, p, 12)
	if err := CheckHeaderChain(p, hs); err != nil {
		t.Fatal(err)
	}
	if CheckHeaderChain(p, nil) != nil || CheckHeaderChain(p, hs[:1]) != nil {
		t.Fatal("empty and anchor-only runs are valid")
	}
	// Any suffix is valid with its first header as the anchor.
	for i := range hs {
		if err := CheckHeaderChain(p, hs[i:]); err != nil {
			t.Fatalf("suffix %d: %v", i, err)
		}
	}
	// The anchor itself is not checked.
	anchor := append([]core.Header{}, hs[3:]...)
	anchor[0].Difficulty = 0
	anchor[1].Prev = core.HeaderHash(anchor[0])
	anchor[1] = mine(t, anchor[1])
	if err := CheckHeaderChain(p, anchor[:2]); err != nil {
		t.Fatalf("anchor must not be checked: %v", err)
	}

	// A broken link anywhere is found, and reported at its index.
	for i := 1; i < len(hs); i++ {
		bad := append([]core.Header{}, hs...)
		bad[i].Prev[0] ^= 0x80
		err := CheckHeaderChain(p, bad)
		if !errors.Is(err, ErrLinkage) || !strings.HasPrefix(err.Error(), "header "+strconv.Itoa(i)+":") {
			t.Fatalf("link broken at %d: %v", i, err)
		}
	}
	// A header altered after mining breaks at itself (work, or linkage of
	// the next header), never later.
	for i := 1; i < len(hs); i++ {
		bad := append([]core.Header{}, hs...)
		bad[i].Timestamp++
		err := CheckHeaderChain(p, bad)
		if err == nil {
			t.Fatalf("altered header %d accepted", i)
		}
		at := "header " + strconv.Itoa(i) + ":"
		next := "header " + strconv.Itoa(i+1) + ":"
		if !strings.HasPrefix(err.Error(), at) && !strings.HasPrefix(err.Error(), next) {
			t.Fatalf("altered header %d reported as %v", i, err)
		}
	}
	// A chain mined at another difficulty is rejected.
	q := p
	q.Difficulty = p.Difficulty + 1
	if err := CheckHeaderChain(q, hs); !errors.Is(err, ErrDifficulty) {
		t.Fatalf("other difficulty: %v", err)
	}
}

func TestPrefer(t *testing.T) {
	cases := []struct {
		cand, cur uint64
		want      bool
	}{
		{2, 1, true}, {1, 1, false}, {1, 2, false}, {0, 0, false},
		{math.MaxUint64, math.MaxUint64 - 1, true}, {math.MaxUint64, math.MaxUint64, false},
	}
	for _, c := range cases {
		if Prefer(c.cand, c.cur) != c.want {
			t.Fatalf("Prefer(%d, %d) != %v", c.cand, c.cur, c.want)
		}
	}
}
