package core

import (
	"errors"
	"math"
	"math/bits"
	"math/rand"
	"sync"
	"testing"
)

// refLZB counts leading zero bits in Go, independently of the C++ code.
func refLZB(h Hash) int {
	n := 0
	for _, b := range h {
		if b != 0 {
			return n + bits.LeadingZeros8(b)
		}
		n += 8
	}
	return n
}

func randomHeader(r *rand.Rand) Header {
	var h Header
	h.Version = r.Uint32()
	r.Read(h.Prev[:])
	r.Read(h.MerkleRoot[:])
	h.Timestamp = r.Int63()
	h.Difficulty = r.Uint32()
	h.Nonce = r.Uint64()
	return h
}

// refSearch scans nonce by nonce through HeaderHash (D1's encoding and hash).
func refSearch(h Header, start, count uint64, d uint32) (uint64, Hash, bool) {
	for i := uint64(0); i < count; i++ {
		h.Nonce = start + i
		hh := HeaderHash(h)
		if refLZB(hh) >= int(d) {
			return h.Nonce, hh, true
		}
		if h.Nonce == math.MaxUint64 {
			break
		}
	}
	return 0, Hash{}, false
}

func TestPowCheck(t *testing.T) {
	var zero Hash
	if !PowCheck(zero, 0) || !PowCheck(zero, MaxDifficulty) || PowCheck(zero, MaxDifficulty+1) {
		t.Fatal("zero hash: wrong result at 0, 256 or 257")
	}
	if PowCheck(zero, math.MaxUint32) {
		t.Fatal("difficulty MaxUint32 must never be met")
	}
	for p := 0; p < 256; p++ { // one bit set at position p: exactly p zeros
		var h Hash
		h[p/8] = 0x80 >> (p % 8)
		if !PowCheck(h, uint32(p)) || PowCheck(h, uint32(p+1)) {
			t.Fatalf("bit %d: boundary wrong", p)
		}
	}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		var h Hash
		r.Read(h[:])
		for k, z := 0, r.Intn(4); k < z; k++ {
			h[k] = 0
		}
		for d := 0; d <= 64; d++ {
			if PowCheck(h, uint32(d)) != (refLZB(h) >= d) {
				t.Fatalf("%s at d=%d", h, d)
			}
		}
	}
}

// PowSearch returns the reference's first solution in random ranges, and its
// hash is HeaderHash of the header with that nonce.
func TestPowSearchMatchesReference(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for i := 0; i < 30; i++ {
		h := randomHeader(r)
		d := uint32(r.Intn(9)) // 0..8
		start := r.Uint64()
		if i%3 == 0 {
			start = math.MaxUint64 - uint64(r.Intn(40))
		}
		count := uint64(r.Intn(3000))
		n, hash, found, err := PowSearch(h, start, count, d)
		if err != nil {
			t.Fatal(err)
		}
		wn, wh, wf := refSearch(h, start, count, d)
		if found != wf || n != wn || hash != wh {
			t.Fatalf("case %d: got (%d,%v) want (%d,%v)", i, n, found, wn, wf)
		}
		if found {
			h.Nonce = n
			if HeaderHash(h) != hash || !PowCheck(hash, d) {
				t.Fatalf("case %d: returned hash is not the solved header's hash", i)
			}
		}
	}
}

func TestPowSearchArguments(t *testing.T) {
	h := randomHeader(rand.New(rand.NewSource(3)))
	if _, _, _, err := PowSearch(h, 0, 10, MaxDifficulty+1); !errors.Is(err, ErrArg) {
		t.Fatalf("d=257: err %v, want ErrArg", err)
	}
	if _, _, found, err := PowSearch(h, 5, 0, 0); found || err != nil {
		t.Fatal("count 0 must find nothing without error")
	}
	if n, _, found, _ := PowSearch(h, 9, 3, 0); !found || n != 9 {
		t.Fatal("d=0 must return the first nonce of the range")
	}
	if n, _, found, _ := PowSearch(h, math.MaxUint64, math.MaxUint64, 0); !found || n != math.MaxUint64 {
		t.Fatal("search at the top of the nonce space")
	}
	// The header's own nonce is ignored.
	h2 := h
	h2.Nonce = ^h.Nonce
	a, _, _, _ := PowSearch(h, 0, 5000, 8)
	b, _, _, _ := PowSearch(h2, 0, 5000, 8)
	if a != b {
		t.Fatal("result depends on h.Nonce")
	}
}

// Consecutive 65,536-nonce chunks find the same first solution as one call
// over the whole range: no nonce is skipped or repeated at a boundary.
func TestPowSearchChunks(t *testing.T) {
	const c = 1 << 16
	r := rand.New(rand.NewSource(4))
	for i := 0; i < 3; i++ {
		h := randomHeader(r)
		const d = 15
		wn, wh, wf, _ := PowSearch(h, 0, 32*c, d)
		var n uint64
		var hash Hash
		found := false
		for s := uint64(0); !found && s < 32*c; s += c {
			n, hash, found, _ = PowSearch(h, s, c, d)
		}
		if found != wf || n != wn || hash != wh {
			t.Fatalf("header %d: chunked (%d,%v) != whole (%d,%v)", i, n, found, wn, wf)
		}
	}
}

// PowSearch takes no lock: concurrent calls give the sequential results.
func TestPowSearchConcurrent(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	const g = 8
	hs := make([]Header, g)
	want := make([]uint64, g)
	for i := range hs {
		hs[i] = randomHeader(r)
		want[i], _, _, _ = PowSearch(hs[i], 0, 1<<14, 10)
	}
	var wg sync.WaitGroup
	got := make([]uint64, g)
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _, _, _ = PowSearch(hs[i], 0, 1<<14, 10)
		}(i)
	}
	wg.Wait()
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("goroutine %d: %d, want %d", i, got[i], want[i])
		}
	}
}
