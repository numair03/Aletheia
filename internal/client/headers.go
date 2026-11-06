// Package client is the light client. It stores only block headers, keeps
// them in step with the full nodes through D2's block locator (DR-14), and
// verifies Merkle inclusion proofs itself, against the Merkle roots of its
// own validated headers (SPV, M-R7, M-R8).
//
// The package contains no consensus rule and no locator algorithm of its
// own: locators come from consensus.BuildLocator, header validation from
// consensus.CheckHeaderChain, chain choice from consensus.Prefer, and proof
// verification from D1's verifier through core.MerkleVerify.
package client

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Anon-HP/Aletheia/internal/api"
	"github.com/Anon-HP/Aletheia/internal/consensus"
	"github.com/Anon-HP/Aletheia/internal/core"
)

// Header synchronisation errors. A rejected answer never changes the stored
// headers.
var (
	// ErrBadFork: the node's fork point is not a height of the locator the
	// client sent. A node following DR-14 always answers with the height of
	// one of the entries, so any other value is a protocol violation.
	ErrBadFork = errors.New("client: fork point is not an entry of the locator sent")
	// ErrInvalidSegment: the headers after the fork point failed D2's
	// validation (linkage to the client's own header at the fork point,
	// fixed difficulty, proof of work). The error also wraps the
	// consensus error, e.g. consensus.ErrWork.
	ErrInvalidSegment = errors.New("client: header segment failed validation")
)

// SyncResult describes one header synchronisation.
type SyncResult struct {
	Node     string // the node that answered ("" if none did)
	Fork     uint64 // the fork point the node returned
	Received int    // headers received after the fork point
	Adopted  bool   // true if the received chain replaced the stored one
	Height   uint64 // the client's tip height after the synchronisation
}

// fetchFunc sends a locator to a node and returns the node's fork point and
// its headers after it (GET /headers?locator=…).
type fetchFunc func(ctx context.Context, loc []consensus.LocatorEntry) (fork uint64, hs []core.Header, err error)

// HeaderChain is the light client's header store: the headers 0..tip of the
// best valid chain the client knows. It starts from the network's genesis
// header (Params.Genesis), which is identical for every node and client
// (CD-27) and is the trusted anchor of everything stored later. Every header
// above genesis has been checked by consensus.CheckHeader against its
// predecessor (linkage, fixed difficulty, proof of work) before it was
// stored. Only headers are kept, never transactions or balances (R6).
//
// Storage is a slice indexed by height (headers[h] is the header at height
// h); its hash linkage is what CheckHeaderChain verifies. Memory is 88 bytes
// of fields per block.
//
// Safe for concurrent use. Synchronisations run one at a time (syncMu), so a
// locator, the answer to it, and the store it is applied to always belong
// together; readers are never blocked by the network part of a sync.
type HeaderChain struct {
	p consensus.Params

	syncMu sync.Mutex // serialises synchronisations: locator → fetch → apply

	mu      sync.RWMutex
	headers []core.Header // headers[0] = p.Genesis; never empty
}

// NewHeaderChain returns a store holding only the genesis header of p.
func NewHeaderChain(p consensus.Params) *HeaderChain {
	return &HeaderChain{p: p, headers: []core.Header{p.Genesis}}
}

// Height returns the height of the stored tip (0: only genesis).
func (c *HeaderChain) Height() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return uint64(len(c.headers) - 1)
}

// Tip returns the stored tip's height and header.
func (c *HeaderChain) Tip() (uint64, core.Header) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h := len(c.headers) - 1
	return uint64(h), c.headers[h]
}

// Header returns the stored header at height h, or false if the client has
// no header at that height.
func (c *HeaderChain) Header(h uint64) (core.Header, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if h >= uint64(len(c.headers)) {
		return core.Header{}, false
	}
	return c.headers[h], true
}

// Headers returns a copy of all stored headers, genesis first.
func (c *HeaderChain) Headers() []core.Header {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]core.Header(nil), c.headers...)
}

// Locator returns the block locator of the stored chain, built by D2's
// consensus.BuildLocator: the tip and the 9 heights below it, then gaps
// doubling down to genesis (O(log n) entries). Block hashes are recomputed
// from the stored headers by the C++ core.
func (c *HeaderChain) Locator() []consensus.LocatorEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return consensus.BuildLocator(uint64(len(c.headers)-1), func(h uint64) core.Hash {
		return core.HeaderHash(c.headers[h])
	})
}

// Sync brings the store up to date with one node, in one round trip
// (GET /headers?locator=…, DR-14):
//
//  1. send the locator of the stored chain;
//  2. the node answers with its fork point and its headers after it;
//  3. the fork point must be a height of that locator;
//  4. the segment, anchored on the client's own header at the fork point,
//     must pass consensus.CheckHeaderChain;
//  5. the stored headers above the fork point are replaced by the segment
//     only if consensus.Prefer accepts the resulting chain (strictly
//     longer; on a tie the stored chain is kept).
//
// A failed request or a rejected answer returns an error and leaves the
// store unchanged; a valid answer that is not longer is not an error
// (Adopted is false). Light clients reach nodes through the selector
// instead (LightClient.Sync); this form serves a fixed node.
func (c *HeaderChain) Sync(ctx context.Context, ac *api.Client, node string) (SyncResult, error) {
	res, err := c.sync(ctx, func(ctx context.Context, loc []consensus.LocatorEntry) (uint64, []core.Header, error) {
		return ac.HeadersAfter(ctx, node, loc)
	})
	res.Node = node
	return res, err
}

// sync runs one synchronisation with fetch: the locator is built, sent and
// its answer applied while syncMu is held, so the store cannot change in
// between.
func (c *HeaderChain) sync(ctx context.Context, fetch fetchFunc) (SyncResult, error) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	loc := c.Locator()
	fork, seg, err := fetch(ctx, loc)
	if err != nil {
		return SyncResult{Height: c.Height()}, err
	}
	return c.apply(loc, fork, seg)
}

// apply checks a node's answer (fork, seg) to the locator loc and adopts it
// if it describes a longer valid chain. Requires syncMu: loc was built from
// the current store, and no other writer exists while it is held, so the
// checks can run without the write lock.
func (c *HeaderChain) apply(loc []consensus.LocatorEntry, fork uint64, seg []core.Header) (SyncResult, error) {
	res := SyncResult{Fork: fork, Received: len(seg)}
	c.mu.RLock()
	height := uint64(len(c.headers) - 1)
	inLoc := false
	for _, e := range loc {
		if e.Height == fork {
			inLoc = true
			break
		}
	}
	var anchor core.Header
	if inLoc && fork <= height {
		anchor = c.headers[fork]
	}
	c.mu.RUnlock()
	res.Height = height

	// The fork point: the node claims to hold our block at this height, so
	// it must be one we asked about (all locator heights are <= our tip).
	if !inLoc || fork > height {
		return res, fmt.Errorf("%w: node returned fork point %d", ErrBadFork, fork)
	}
	// The segment: linkage to our own header at the fork point, then every
	// header against its predecessor (D2's rules; hashes recomputed by the
	// C++ core, never taken from the node).
	chain := make([]core.Header, 0, len(seg)+1)
	chain = append(chain, anchor)
	chain = append(chain, seg...)
	if err := consensus.CheckHeaderChain(c.p, chain); err != nil {
		return res, fmt.Errorf("%w: %w", ErrInvalidSegment, err)
	}
	// Chain choice: D2's rule over tip heights (with a fixed difficulty, the
	// longer chain is the one with more work), as the full node does.
	if !consensus.Prefer(fork+uint64(len(seg)), height) {
		return res, nil
	}
	c.mu.Lock()
	c.headers = append(c.headers[:fork+1], seg...)
	res.Height = uint64(len(c.headers) - 1)
	c.mu.Unlock()
	res.Adopted = true
	return res, nil
}
