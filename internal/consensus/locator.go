package consensus

import (
	"errors"
	"fmt"

	"github.com/Anon-HP/Aletheia/internal/core"
)

// Block locator (DR-14): how two parties holding different chains find a
// common ancestor in one round trip.
//
// The requester sends a locator: (height, hash) pairs of its own chain,
// dense near its tip and exponentially sparser towards genesis. The
// responder walks the entries from the top and returns the first height at
// which its own chain has the same hash. Equal block hashes at height h mean
// identical chains on 0..h (each header commits to its predecessor's hash),
// so that height is a common ancestor, and the responder then sends only the
// blocks or headers after it.
//
// If the real last common ancestor is k blocks below the requester's tip,
// the locator has an entry at most about 2k + 10 blocks below the tip that
// is still on the common part, so the response over-fetches O(k) blocks the
// requester already has (identical, hence harmless). Heights travel with the
// hashes, so the responder needs no hash index: one HeaderAt and one
// HeaderHash per entry.
//
// This file is the only implementation of both halves. Full nodes build and
// resolve locators through internal/node; the light client builds its own
// with BuildLocator and never resolves one.

const (
	// LocatorDense is the number of consecutive heights at the top.
	LocatorDense = 10
	// MaxLocatorEntries bounds a locator a responder accepts. LocatorHeights
	// never exceeds 10 + 64 + 1 = 75 entries, even at height 2^64-1.
	MaxLocatorEntries = 128
)

// LocatorEntry is one (height, block hash) pair of the requester's chain.
type LocatorEntry struct {
	Height uint64
	Hash   core.Hash
}

// Locator resolution errors. A malformed locator is the requester's fault
// (HTTP 400); a genesis mismatch means the two parties are on different
// networks (HTTP 409).
var (
	ErrBadLocator      = errors.New("consensus: malformed locator")
	ErrGenesisMismatch = errors.New("consensus: genesis block differs")
)

// LocatorHeights returns the heights a locator for a chain with the given
// tip height contains: tip, tip-1, ..., tip-9, then gaps of 2, 4, 8, ...,
// and always 0 (genesis) last. The heights are strictly decreasing, and
// there are at most LocatorDense + ceil(log2(tip+1)) + 1 of them.
func LocatorHeights(tip uint64) []uint64 {
	hs := make([]uint64, 0, LocatorDense+16)
	h, step := tip, uint64(1)
	for {
		hs = append(hs, h)
		if h == 0 {
			return hs
		}
		if len(hs) >= LocatorDense {
			step *= 2 // cannot overflow: h reaches 0 before step passes 2^63
		}
		if h > step {
			h -= step
		} else {
			h = 0
		}
	}
}

// BuildLocator returns the locator of a chain whose tip is at height tip;
// hashAt(h) must return the block hash at height h (0 <= h <= tip). It is
// called once per entry, with heights in decreasing order.
func BuildLocator(tip uint64, hashAt func(uint64) core.Hash) []LocatorEntry {
	heights := LocatorHeights(tip)
	loc := make([]LocatorEntry, len(heights))
	for i, h := range heights {
		loc[i] = LocatorEntry{Height: h, Hash: hashAt(h)}
	}
	return loc
}

// ForkPoint resolves a requester's locator against the responder's chain
// (tip height tip; hashAt(h) is the responder's block hash at height h). It
// returns the height of the first, that is highest, entry whose hash equals
// the responder's hash at that height: a common ancestor of both chains.
//
// The locator is validated completely before any lookup: it must have
// 1..MaxLocatorEntries entries, strictly decreasing heights, and end at
// height 0; otherwise the error wraps ErrBadLocator. It need not have been
// produced by BuildLocator. Entries above tip are skipped (the responder's
// chain is shorter there), so hashAt is called only for heights <= tip, at
// most once per entry. If no entry matches, the hashes differ even at
// genesis, and the result is ErrGenesisMismatch.
func ForkPoint(loc []LocatorEntry, tip uint64, hashAt func(uint64) core.Hash) (uint64, error) {
	if len(loc) == 0 {
		return 0, fmt.Errorf("%w: no entries", ErrBadLocator)
	}
	if len(loc) > MaxLocatorEntries {
		return 0, fmt.Errorf("%w: %d entries, at most %d allowed", ErrBadLocator, len(loc), MaxLocatorEntries)
	}
	for i := 1; i < len(loc); i++ {
		if loc[i].Height >= loc[i-1].Height {
			return 0, fmt.Errorf("%w: heights not strictly decreasing at entry %d", ErrBadLocator, i)
		}
	}
	if loc[len(loc)-1].Height != 0 {
		return 0, fmt.Errorf("%w: last entry is not genesis", ErrBadLocator)
	}
	for _, e := range loc {
		if e.Height > tip {
			continue
		}
		if hashAt(e.Height) == e.Hash {
			return e.Height, nil
		}
	}
	return 0, ErrGenesisMismatch
}
