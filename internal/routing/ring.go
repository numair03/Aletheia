// Package routing distributes light-client requests across full nodes with a
// consistent-hash ring, and fails over clockwise to the next node when one
// does not answer.
package routing

import (
	"errors"
	"slices"
	"strconv"
	"sync"
)

// ErrEmpty is returned when the ring has no nodes.
var ErrEmpty = errors.New("routing: ring has no nodes")

// point is one virtual node: a position on the ring owned by a physical node.
type point struct {
	hash uint64
	node string
}

// Ring is a consistent-hash ring with virtual nodes, written from scratch.
//
// Every physical node is placed on a 64-bit circle at `vnodes` pseudo-random
// positions. A key belongs to the first position at or clockwise after the
// key's own hash (wrapping past the top). Adding or removing a node only moves
// the keys in the arcs that node owned (about 1/N of them); every other key
// keeps its owner. Many virtual nodes per node even out the arc lengths.
//
// Structure: a slice of points sorted by hash, searched with a hand-written
// binary search: lookup is O(log(N·vnodes)); Add/Remove rebuild the sorted
// slice in O(N·vnodes·log(N·vnodes)), which is rare (membership changes).
// Safe for concurrent use.
type Ring struct {
	mu     sync.RWMutex
	vnodes int
	points []point // sorted by hash, then node (ties are practically impossible)
	nodes  []string
}

// New returns a ring with the given number of virtual nodes per physical node
// (at least 1) containing `nodes`.
func New(vnodes int, nodes ...string) *Ring {
	if vnodes < 1 {
		vnodes = 1
	}
	r := &Ring{vnodes: vnodes}
	for _, n := range nodes {
		r.Add(n)
	}
	return r
}

// Add places a node on the ring. Adding a node already present does nothing.
func (r *Ring) Add(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.Contains(r.nodes, node) {
		return
	}
	r.nodes = append(r.nodes, node)
	for i := 0; i < r.vnodes; i++ {
		r.points = append(r.points, point{hash: hashKey(node + "#" + strconv.Itoa(i)), node: node})
	}
	slices.SortFunc(r.points, func(a, b point) int {
		switch {
		case a.hash < b.hash:
			return -1
		case a.hash > b.hash:
			return 1
		case a.node < b.node:
			return -1
		case a.node > b.node:
			return 1
		}
		return 0
	})
}

// Remove takes a node off the ring. Its keys move to their clockwise
// successors; all other keys keep their owner.
func (r *Ring) Remove(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes = slices.DeleteFunc(r.nodes, func(n string) bool { return n == node })
	r.points = slices.DeleteFunc(r.points, func(p point) bool { return p.node == node })
}

// Nodes returns the physical nodes, in the order they were added.
func (r *Ring) Nodes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.nodes)
}

// Owner returns the node responsible for key.
func (r *Ring) Owner(key string) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.points) == 0 {
		return "", ErrEmpty
	}
	return r.points[r.successor(hashKey(key))].node, nil
}

// Candidates returns every physical node exactly once, in failover order for
// key: the owner first, then the distinct owners met walking clockwise.
func (r *Ring) Candidates(key string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.nodes))
	if len(r.points) == 0 {
		return out
	}
	start := r.successor(hashKey(key))
	for i := 0; i < len(r.points) && len(out) < len(r.nodes); i++ {
		n := r.points[(start+i)%len(r.points)].node
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// Try calls fn with each candidate for key in failover order until one
// succeeds, and returns the node that answered. If every node fails, it
// returns the joined errors. fn decides what counts as failure (an error or
// a timeout from the HTTP client).
func (r *Ring) Try(key string, fn func(node string) error) (string, error) {
	candidates := r.Candidates(key) // snapshot: fn runs without holding the lock
	if len(candidates) == 0 {
		return "", ErrEmpty
	}
	var errs []error
	for _, n := range candidates {
		err := fn(n)
		if err == nil {
			return n, nil
		}
		errs = append(errs, errors.New(n+": "+err.Error()))
	}
	return "", errors.Join(errs...)
}

// successor returns the index of the first point with hash >= h, wrapping to
// 0 past the last point. Binary search over the sorted points; requires
// len(r.points) > 0.
func (r *Ring) successor(h uint64) int {
	lo, hi := 0, len(r.points) // invariant: answer is in [lo, hi]
	for lo < hi {
		mid := lo + (hi-lo)/2
		if r.points[mid].hash < h {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == len(r.points) {
		return 0 // past the top of the circle: wrap around
	}
	return lo
}

// hashKey places a string on the ring: hand-written FNV-1a followed by a
// final avalanche mix. FNV-1a alone spreads short, similar strings ("node#1",
// "node#2") unevenly across the high bits; the mix (from SplitMix64) makes
// every input bit affect every output bit, so virtual nodes land uniformly.
func hashKey(s string) uint64 { return mix64(fnv1a(s)) }

// fnv1a is the 64-bit FNV-1a hash.
func fnv1a(s string) uint64 {
	h := uint64(14695981039346656037) // FNV offset basis
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211 // FNV prime
	}
	return h
}

// mix64 is the SplitMix64 finaliser, a bijection on uint64.
func mix64(h uint64) uint64 {
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	return h
}
