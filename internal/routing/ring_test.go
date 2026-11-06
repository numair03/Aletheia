package routing

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"strings"
	"sync"
	"testing"
)

func keys(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("client-%d", i)
	}
	return out
}

func owners(t *testing.T, r *Ring, ks []string) map[string]string {
	t.Helper()
	m := make(map[string]string, len(ks)) // test bookkeeping only
	for _, k := range ks {
		o, err := r.Owner(k)
		if err != nil {
			t.Fatal(err)
		}
		m[k] = o
	}
	return m
}

// The hand-written FNV-1a matches the standard library's (used here only as
// a reference).
func TestFNV1aMatchesReference(t *testing.T) {
	for _, s := range []string{"", "a", "foobar", "node-1#0", strings.Repeat("x", 1000)} {
		ref := fnv.New64a()
		ref.Write([]byte(s))
		if got, want := fnv1a(s), ref.Sum64(); got != want {
			t.Errorf("fnv1a(%q) = %x, want %x", s, got, want)
		}
	}
}

func TestEmptyRing(t *testing.T) {
	r := New(10)
	if _, err := r.Owner("k"); !errors.Is(err, ErrEmpty) {
		t.Errorf("Owner on empty ring: err = %v, want ErrEmpty", err)
	}
	if c := r.Candidates("k"); len(c) != 0 {
		t.Errorf("Candidates on empty ring = %v", c)
	}
	if _, err := r.Try("k", func(string) error { return nil }); !errors.Is(err, ErrEmpty) {
		t.Errorf("Try on empty ring: err = %v, want ErrEmpty", err)
	}
	r.Add("a")
	r.Remove("a")
	if _, err := r.Owner("k"); !errors.Is(err, ErrEmpty) {
		t.Errorf("after removing the only node: err = %v, want ErrEmpty", err)
	}
}

func TestSingleNodeOwnsEverything(t *testing.T) {
	r := New(5, "only")
	for _, k := range keys(100) {
		if o, _ := r.Owner(k); o != "only" {
			t.Fatalf("Owner(%q) = %q", k, o)
		}
	}
}

// The binary search agrees with a linear scan, including keys that hash past
// the last point and must wrap to the first.
func TestSuccessorMatchesLinearScan(t *testing.T) {
	r := New(20, "a", "b", "c")
	linear := func(h uint64) int {
		for i, p := range r.points {
			if p.hash >= h {
				return i
			}
		}
		return 0
	}
	rng := rand.New(rand.NewSource(1))
	probes := []uint64{0, ^uint64(0), r.points[0].hash, r.points[len(r.points)-1].hash,
		r.points[len(r.points)-1].hash + 1, r.points[5].hash - 1}
	for i := 0; i < 10000; i++ {
		probes = append(probes, rng.Uint64())
	}
	for _, h := range probes {
		if got, want := r.successor(h), linear(h); got != want {
			t.Fatalf("successor(%x) = %d, want %d", h, got, want)
		}
	}
	for i := 1; i < len(r.points); i++ {
		if r.points[i-1].hash > r.points[i].hash {
			t.Fatal("points are not sorted")
		}
	}
}

// With 150 virtual nodes each, three nodes each get close to a third of keys.
func TestDistributionIsBalanced(t *testing.T) {
	r := New(150, "node-a", "node-b", "node-c")
	ks := keys(30000)
	count := map[string]int{}
	for _, o := range owners(t, r, ks) {
		count[o]++
	}
	for _, n := range r.Nodes() {
		share := float64(count[n]) / float64(len(ks))
		if share < 0.27 || share > 0.40 {
			t.Errorf("node %s owns %.3f of keys, want about 1/3 (counts %v)", n, share, count)
		}
	}
}

// Adding a node moves only keys that now belong to it (about 1/4 of them);
// removing it again restores every original owner.
func TestMembershipChangesMoveFewKeys(t *testing.T) {
	r := New(150, "node-a", "node-b", "node-c")
	ks := keys(20000)
	before := owners(t, r, ks)

	r.Add("node-d")
	after := owners(t, r, ks)
	moved := 0
	for _, k := range ks {
		if before[k] != after[k] {
			moved++
			if after[k] != "node-d" {
				t.Fatalf("key %q moved from %s to %s, not to the new node", k, before[k], after[k])
			}
		}
	}
	if frac := float64(moved) / float64(len(ks)); frac < 0.18 || frac > 0.32 {
		t.Errorf("adding a 4th node moved %.3f of keys, want about 0.25", frac)
	}

	r.Remove("node-d")
	restored := owners(t, r, ks)
	for _, k := range ks {
		if restored[k] != before[k] {
			t.Fatalf("key %q owner %s after removal, want %s", k, restored[k], before[k])
		}
	}

	// Removing an original node only moves that node's keys.
	r.Remove("node-b")
	for k, o := range owners(t, r, ks) {
		if before[k] != "node-b" && o != before[k] {
			t.Fatalf("key %q moved although its owner %s stayed", k, before[k])
		}
		if o == "node-b" {
			t.Fatal("removed node still owns keys")
		}
	}
}

// Placement depends only on membership, not on insertion order, so every
// client computes the same owner from the same node list.
func TestOrderIndependent(t *testing.T) {
	ks := keys(2000)
	a := owners(t, New(50, "x", "y", "z"), ks)
	b := owners(t, New(50, "z", "x", "y"), ks)
	for _, k := range ks {
		if a[k] != b[k] {
			t.Fatalf("key %q: %s vs %s", k, a[k], b[k])
		}
	}
}

func TestDuplicateAddAndUnknownRemove(t *testing.T) {
	r := New(10, "a", "b")
	r.Add("a")
	r.Remove("nope")
	if len(r.points) != 20 || len(r.Nodes()) != 2 {
		t.Fatalf("points=%d nodes=%v", len(r.points), r.Nodes())
	}
}

func TestCandidates(t *testing.T) {
	r := New(50, "a", "b", "c", "d")
	for _, k := range keys(500) {
		c := r.Candidates(k)
		if len(c) != 4 {
			t.Fatalf("Candidates(%q) = %v", k, c)
		}
		seen := map[string]bool{}
		for _, n := range c {
			if seen[n] {
				t.Fatalf("duplicate in %v", c)
			}
			seen[n] = true
		}
		if o, _ := r.Owner(k); c[0] != o {
			t.Fatalf("first candidate %s != owner %s", c[0], o)
		}
		// The second candidate is the owner once the first node is gone:
		// failover order matches what the ring would do after a removal.
		r2 := New(50, "a", "b", "c", "d")
		r2.Remove(c[0])
		if o2, _ := r2.Owner(k); o2 != c[1] {
			t.Fatalf("key %q: after removing %s owner is %s, want %s", k, c[0], o2, c[1])
		}
	}
}

func TestTryFailsOver(t *testing.T) {
	r := New(50, "a", "b", "c")
	const key = "client-7"
	order := r.Candidates(key)

	var called []string
	got, err := r.Try(key, func(n string) error {
		called = append(called, n)
		if n == order[0] || n == order[1] {
			return errors.New("timeout")
		}
		return nil
	})
	if err != nil || got != order[2] {
		t.Fatalf("Try = %q, %v; want %q", got, err, order[2])
	}
	if strings.Join(called, ",") != strings.Join(order, ",") {
		t.Fatalf("tried %v, want %v", called, order)
	}

	// First node healthy: no failover.
	if got, err := r.Try(key, func(string) error { return nil }); err != nil || got != order[0] {
		t.Fatalf("Try = %q, %v; want %q", got, err, order[0])
	}

	// Every node down: the error names each node.
	_, err = r.Try(key, func(n string) error { return errors.New("down") })
	if err == nil {
		t.Fatal("Try succeeded with every node down")
	}
	for _, n := range order {
		if !strings.Contains(err.Error(), n+": down") {
			t.Errorf("error %q does not mention %s", err, n)
		}
	}
}

// Lookups run concurrently with membership changes (run with -race).
func TestConcurrentUse(t *testing.T) {
	r := New(50, "a", "b", "c")
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				k := fmt.Sprintf("k-%d-%d", g, i)
				if _, err := r.Owner(k); err != nil {
					t.Error(err)
					return
				}
				r.Candidates(k)
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			r.Add("d")
			r.Remove("d")
		}
	}()
	wg.Wait()
}
