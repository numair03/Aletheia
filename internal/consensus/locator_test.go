package consensus

import (
	"errors"
	"math"
	"math/bits"
	"math/rand"
	"reflect"
	"testing"

	"github.com/Anon-HP/Aletheia/internal/core"
)

// lenBound is DR-14's bound: 10 + ceil(log2(tip+1)) + 1.
func lenBound(tip uint64) int {
	if tip == math.MaxUint64 {
		return LocatorDense + 64 + 1
	}
	return LocatorDense + bits.Len64(tip) + 1 // bits.Len64(x) = ceil(log2(x+1))
}

// checkShape verifies every structural property of LocatorHeights(tip).
func checkShape(t *testing.T, tip uint64) {
	t.Helper()
	hs := LocatorHeights(tip)
	if hs[0] != tip || hs[len(hs)-1] != 0 {
		t.Fatalf("tip %d: first %d, last %d", tip, hs[0], hs[len(hs)-1])
	}
	if len(hs) > lenBound(tip) || len(hs) > MaxLocatorEntries {
		t.Fatalf("tip %d: %d entries, bound %d", tip, len(hs), lenBound(tip))
	}
	gap := uint64(2)
	for i := 1; i < len(hs); i++ {
		if hs[i] >= hs[i-1] {
			t.Fatalf("tip %d: not strictly decreasing at %d", tip, i)
		}
		d := hs[i-1] - hs[i]
		switch {
		case i < LocatorDense: // dense part
			if d != 1 {
				t.Fatalf("tip %d: gap %d at dense entry %d", tip, d, i)
			}
		case hs[i] == 0: // the final step may be clamped at genesis
			if d > gap {
				t.Fatalf("tip %d: final gap %d > %d", tip, d, gap)
			}
		default: // doubling gaps 2, 4, 8, ...
			if d != gap {
				t.Fatalf("tip %d: gap %d at entry %d, want %d", tip, d, i, gap)
			}
			gap *= 2
		}
	}
}

func TestLocatorHeightsExamples(t *testing.T) {
	cases := map[uint64][]uint64{
		0:  {0},
		1:  {1, 0},
		5:  {5, 4, 3, 2, 1, 0},
		9:  {9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
		10: {10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
		11: {11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 0},
		30: {30, 29, 28, 27, 26, 25, 24, 23, 22, 21, 19, 15, 7, 0},
		45: {45, 44, 43, 42, 41, 40, 39, 38, 37, 36, 34, 30, 22, 6, 0},
	}
	for tip, want := range cases {
		if got := LocatorHeights(tip); !reflect.DeepEqual(got, want) {
			t.Fatalf("tip %d: %v, want %v", tip, got, want)
		}
	}
}

// Every tip up to 10^6, plus the top of the uint64 range.
func TestLocatorHeightsShape(t *testing.T) {
	for tip := uint64(0); tip <= 1_000_000; tip++ {
		checkShape(t, tip)
	}
	for _, tip := range []uint64{math.MaxUint64, math.MaxUint64 - 1, 1 << 63, 1<<63 - 1, 1 << 32} {
		checkShape(t, tip)
	}
	if n := len(LocatorHeights(math.MaxUint64)); n > 75 {
		t.Fatalf("MaxUint64: %d entries", n)
	}
}

// randomChain returns n+1 random block hashes (heights 0..n) whose first
// len(prefix) entries are prefix.
func randomChain(r *rand.Rand, prefix []core.Hash, n uint64) []core.Hash {
	c := append([]core.Hash{}, prefix...)
	for uint64(len(c)) <= n {
		var h core.Hash
		r.Read(h[:])
		c = append(c, h)
	}
	return c
}

// lookup returns a hashAt function over c that fails the test on a height
// outside the chain and counts its calls.
func lookup(t *testing.T, c []core.Hash, calls *int) func(uint64) core.Hash {
	return func(h uint64) core.Hash {
		*calls++
		if h >= uint64(len(c)) {
			t.Fatalf("hashAt(%d) above tip %d", h, len(c)-1)
		}
		return c[h]
	}
}

func TestBuildLocator(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, tip := range []uint64{0, 1, 9, 10, 11, 100, 4097} {
		c := randomChain(r, nil, tip)
		calls := 0
		loc := BuildLocator(tip, lookup(t, c, &calls))
		hs := LocatorHeights(tip)
		if len(loc) != len(hs) || calls != len(hs) {
			t.Fatalf("tip %d: %d entries, %d calls, want %d", tip, len(loc), calls, len(hs))
		}
		for i, e := range loc {
			if e.Height != hs[i] || e.Hash != c[e.Height] {
				t.Fatalf("tip %d entry %d", tip, i)
			}
		}
		// A party resolving its own locator finds its own tip.
		if f, err := ForkPoint(loc, tip, lookup(t, c, &calls)); err != nil || f != tip {
			t.Fatalf("tip %d: self fork point %d, %v", tip, f, err)
		}
	}
}

// ForkPoint against a brute-force search over random forked chains. The
// requester (chain a) builds a locator; the responder (chain b) resolves it.
func TestForkPointBruteForce(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for trial := 0; trial < 2000; trial++ {
		// Common part 0..lca, then two independent branches.
		lca := uint64(r.Intn(1500))
		if trial%10 == 0 {
			lca = 0 // only genesis in common
		}
		common := randomChain(r, nil, lca)
		lenA := lca + uint64(r.Intn(1500))
		lenB := lca + uint64(r.Intn(1500))
		switch trial % 7 {
		case 1:
			lenA = lca // a is a prefix of b (a only needs to catch up)
		case 2:
			lenB = lca // b is a prefix of a (responder shorter)
		case 3:
			lenA, lenB = lca, lca // identical chains
		case 4:
			lenB = lca + uint64(r.Intn(3)) // shallow fork near the tips
			lenA = lca + uint64(r.Intn(3))
		}
		a := randomChain(r, common, lenA)
		b := randomChain(r, common, lenB)
		tipA, tipB := uint64(len(a)-1), uint64(len(b)-1)

		// Brute force: the last common height (equal hashes at h imply equal
		// on 0..h), and the highest locator height at or below it.
		bf := uint64(0)
		for h := uint64(0); h <= tipA && h <= tipB; h++ {
			if a[h] == b[h] {
				bf = h
			}
		}
		if bf != lca {
			t.Fatalf("trial %d: brute-force lca %d != constructed %d", trial, bf, lca)
		}
		want := uint64(0)
		for _, h := range LocatorHeights(tipA) {
			if h <= lca {
				want = h
				break
			}
		}

		var callsA, callsB int
		loc := BuildLocator(tipA, lookup(t, a, &callsA))
		got, err := ForkPoint(loc, tipB, lookup(t, b, &callsB))
		if err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
		if got != want {
			t.Fatalf("trial %d: fork point %d, want %d (lca %d, tips %d/%d)", trial, got, want, lca, tipA, tipB)
		}
		if a[got] != b[got] || got > lca {
			t.Fatalf("trial %d: %d is not a common ancestor", trial, got)
		}
		// Over-fetch bound: with the fork k blocks below a's tip, the result
		// is at most 2k + 10 blocks below it.
		if k := tipA - lca; tipA-got > 2*k+10 {
			t.Fatalf("trial %d: %d blocks below tip for fork depth %d", trial, tipA-got, k)
		}
		if callsB > len(loc) {
			t.Fatalf("trial %d: %d lookups for %d entries", trial, callsB, len(loc))
		}
		// Swapping roles finds a common ancestor too.
		locB := BuildLocator(tipB, lookup(t, b, &callsB))
		back, err := ForkPoint(locB, tipA, lookup(t, a, &callsA))
		if err != nil || back > lca || a[back] != b[back] {
			t.Fatalf("trial %d: reverse fork point %d, %v", trial, back, err)
		}
	}
}

// A responder whose chain is shorter skips the entries above its tip and
// never looks up a height it does not have.
func TestForkPointShorterResponder(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	a := randomChain(r, nil, 500)
	for _, tipB := range []uint64{0, 1, 9, 37, 250, 489, 490, 499} {
		b := a[:tipB+1]
		calls := 0
		f, err := ForkPoint(BuildLocator(500, lookup(t, a, new(int))), tipB, lookup(t, b, &calls))
		if err != nil {
			t.Fatal(err)
		}
		// The highest locator height <= tipB, which is on the shared chain.
		want := uint64(0)
		for _, h := range LocatorHeights(500) {
			if h <= tipB {
				want = h
				break
			}
		}
		if f != want || calls != 1 {
			t.Fatalf("tipB %d: fork %d (want %d), %d lookups", tipB, f, want, calls)
		}
	}
}

func TestForkPointMalformed(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	c := randomChain(r, nil, 300)
	valid := BuildLocator(300, func(h uint64) core.Hash { return c[h] })

	decreasing := func(n int) []LocatorEntry {
		loc := make([]LocatorEntry, n)
		for i := range loc {
			loc[i] = LocatorEntry{Height: uint64(n - 1 - i), Hash: c[n-1-i]}
		}
		return loc
	}
	mutate := func(f func([]LocatorEntry) []LocatorEntry) []LocatorEntry {
		return f(append([]LocatorEntry{}, valid...))
	}
	bad := map[string][]LocatorEntry{
		"nil":          nil,
		"empty":        {},
		"129 entries":  decreasing(MaxLocatorEntries + 1),
		"equal":        mutate(func(l []LocatorEntry) []LocatorEntry { l[3].Height = l[2].Height; return l }),
		"increasing":   mutate(func(l []LocatorEntry) []LocatorEntry { l[0], l[1] = l[1], l[0]; return l }),
		"no genesis":   mutate(func(l []LocatorEntry) []LocatorEntry { return l[:len(l)-1] }),
		"ends above 0": {{Height: 5, Hash: c[5]}},
		"zero twice":   {{Height: 0, Hash: c[0]}, {Height: 0, Hash: c[0]}},
	}
	for name, loc := range bad {
		calls := 0
		f, err := ForkPoint(loc, 300, lookup(t, c, &calls))
		if !errors.Is(err, ErrBadLocator) || errors.Is(err, ErrGenesisMismatch) || f != 0 {
			t.Fatalf("%s: %d, %v; want ErrBadLocator", name, f, err)
		}
		if calls != 0 {
			t.Fatalf("%s: %d lookups before validation finished", name, calls)
		}
	}

	// Exactly 128 entries is accepted; any strictly decreasing list ending
	// at genesis is a valid locator, not only BuildLocator's.
	if f, err := ForkPoint(decreasing(MaxLocatorEntries), 300, lookup(t, c, new(int))); err != nil || f != 127 {
		t.Fatalf("128 entries: %d, %v", f, err)
	}
	custom := []LocatorEntry{{300, core.Hash{1}}, {200, c[200]}, {7, c[7]}, {0, c[0]}}
	if f, err := ForkPoint(custom, 300, lookup(t, c, new(int))); err != nil || f != 200 {
		t.Fatalf("custom: %d, %v", f, err)
	}
	if f, err := ForkPoint([]LocatorEntry{{0, c[0]}}, 300, lookup(t, c, new(int))); err != nil || f != 0 {
		t.Fatalf("genesis only: %d, %v", f, err)
	}
	// Entries far above the responder's tip are fine.
	high := []LocatorEntry{{math.MaxUint64, core.Hash{}}, {1 << 40, core.Hash{}}, {0, c[0]}}
	if f, err := ForkPoint(high, 300, lookup(t, c, new(int))); err != nil || f != 0 {
		t.Fatalf("high entries: %d, %v", f, err)
	}
}

func TestForkPointGenesisMismatch(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	a := randomChain(r, nil, 50)
	b := randomChain(r, nil, 80) // different genesis
	f, err := ForkPoint(BuildLocator(50, lookup(t, a, new(int))), 80, lookup(t, b, new(int)))
	if !errors.Is(err, ErrGenesisMismatch) || errors.Is(err, ErrBadLocator) || f != 0 {
		t.Fatalf("got %d, %v; want ErrGenesisMismatch", f, err)
	}
	g := []LocatorEntry{{0, core.Hash{9}}}
	if _, err := ForkPoint(g, 80, lookup(t, b, new(int))); !errors.Is(err, ErrGenesisMismatch) {
		t.Fatalf("genesis-only mismatch: %v", err)
	}
}

// The light-client flow on real mined headers: the requester's locator over
// its own chain, the responder's fork point, and the responder's headers
// after it, which pass CheckHeaderChain anchored at the requester's own
// header at the fork point.
func TestLocatorWithMinedHeaders(t *testing.T) {
	p := testParams()
	p.Difficulty, p.Genesis.Difficulty = 4, 4
	common := chain(t, p, 25)
	a := append([]core.Header{}, common...)
	b := append([]core.Header{}, common...)
	for i := 0; i < 6; i++ { // a: 6 blocks after height 25
		a = append(a, child(t, a[len(a)-1], p.Difficulty, 1000+int64(i)))
	}
	for i := 0; i < 15; i++ { // b: 15 different blocks after height 25
		b = append(b, child(t, b[len(b)-1], p.Difficulty, 2000+int64(i)))
	}
	hashOf := func(hs []core.Header) func(uint64) core.Hash {
		return func(h uint64) core.Hash { return core.HeaderHash(hs[h]) }
	}
	tipA, tipB := uint64(len(a)-1), uint64(len(b)-1)
	f, err := ForkPoint(BuildLocator(tipA, hashOf(a)), tipB, hashOf(b))
	if err != nil {
		t.Fatal(err)
	}
	if f > 25 || core.HeaderHash(a[f]) != core.HeaderHash(b[f]) {
		t.Fatalf("fork point %d", f)
	}
	seg := append([]core.Header{a[f]}, b[f+1:]...)
	if err := CheckHeaderChain(p, seg); err != nil {
		t.Fatal(err)
	}
	if !Prefer(f+1+uint64(len(b)-int(f)-1), tipA+1) {
		t.Fatal("the longer chain must be preferred")
	}
}
