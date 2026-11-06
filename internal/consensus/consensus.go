// Package consensus is the only implementation of Aletheia's consensus rules:
// the network parameters (genesis, fixed difficulty), header validation and
// fork choice. Full nodes run CheckHeader before every Ledger.Append; light
// clients run CheckHeaderChain and Prefer on headers they download. Neither
// side has a copy of these rules of its own.
//
// Fork choice is the longest valid chain by block count. The difficulty is a
// network-wide constant that CheckHeader enforces exactly, so every block
// carries the same expected work (2^d hashes) and the chain with the most
// blocks is also the chain with the most accumulated work.
package consensus

import (
	"errors"
	"fmt"

	"github.com/Anon-HP/Aletheia/internal/core"
)

// Params are the network parameters. Every node and client of one network
// must use identical Params; the genesis block is never mined or sent.
type Params struct {
	// Difficulty is the required number of leading zero bits of every block
	// hash after genesis (fixed; there is no retargeting).
	Difficulty uint32
	// Genesis is block 0: zero Prev and MerkleRoot, no transactions.
	Genesis core.Header
	// Allocations are the genesis balances (there is no mining reward).
	Allocations []core.Alloc
	// MaxTxPerBlock bounds block assembly. Blocks are never empty (M-R5),
	// so miners mine only when the mempool has transactions.
	MaxTxPerBlock int
}

// Default parameter values for the demonstration network.
const (
	// DefaultDifficulty: 2^22 ≈ 4.2 M hashes per block on average. Measured
	// on the development machine at ~1.4 M hashes/s per thread, that is about
	// 0.25 s per block with 12 threads and about 1.5 s with 2 threads.
	DefaultDifficulty    = 22
	DefaultMaxTxPerBlock = 100
	// GenesisTimestamp is 2026-11-10 00:00:00 UTC (Unix seconds).
	GenesisTimestamp = 1794268800
	// DemoAccounts accounts receive DemoBalance each at genesis.
	DemoAccounts = 8
	DemoBalance  = 1_000_000
)

// DemoAccount returns the id of genesis account i: the SHA-256 of
// "aletheia/account/<i>". Account ids are plain 32-byte identifiers (there
// are no signatures), so any client can name them.
func DemoAccount(i int) core.Hash {
	return core.SHA256([]byte(fmt.Sprintf("aletheia/account/%d", i)))
}

// Default returns the demonstration network's parameters. Each call returns
// a fresh Allocations slice.
func Default() Params {
	allocs := make([]core.Alloc, DemoAccounts)
	for i := range allocs {
		allocs[i] = core.Alloc{Account: DemoAccount(i), Balance: DemoBalance}
	}
	return Params{
		Difficulty: DefaultDifficulty,
		Genesis: core.Header{
			Version:    1,
			Timestamp:  GenesisTimestamp,
			Difficulty: DefaultDifficulty,
		},
		Allocations:   allocs,
		MaxTxPerBlock: DefaultMaxTxPerBlock,
	}
}

// Header validation errors. CheckHeader returns exactly one of them;
// CheckHeaderChain wraps one with the index of the failing header.
var (
	ErrLinkage    = errors.New("consensus: prev is not the hash of the previous header")
	ErrDifficulty = errors.New("consensus: difficulty field differs from the network difficulty")
	ErrWork       = errors.New("consensus: block hash does not meet the difficulty")
)

// CheckHeader validates h as the successor of prev:
//  1. linkage: h.Prev equals the block hash of prev;
//  2. fixed difficulty: h.Difficulty equals p.Difficulty (no other value is
//     accepted, higher or lower);
//  3. proof of work: the block hash of h has at least p.Difficulty leading
//     zero bits (the C++ predicate, through core.PowCheck).
//
// The checks run in that order and the first failure is returned. The block
// hash is recomputed here from the header's fields; a hash supplied by a
// miner or a peer is never trusted. Transactions, the Merkle root and
// balances are the ledger's checks (Ledger.Append), not this function's.
func CheckHeader(p Params, prev, h core.Header) error {
	if h.Prev != core.HeaderHash(prev) {
		return ErrLinkage
	}
	if h.Difficulty != p.Difficulty {
		return ErrDifficulty
	}
	if !core.PowCheck(core.HeaderHash(h), p.Difficulty) {
		return ErrWork
	}
	return nil
}

// CheckHeaderChain validates a contiguous run of headers. hs[0] is the
// anchor the run builds on, which the caller already trusts (genesis, or its
// own header at the fork point); it is not checked itself. Every later
// header is checked with CheckHeader against its predecessor. An empty or
// one-element slice is valid. A failure is reported as "header i: <err>",
// wrapping the CheckHeader error (use errors.Is).
func CheckHeaderChain(p Params, hs []core.Header) error {
	for i := 1; i < len(hs); i++ {
		if err := CheckHeader(p, hs[i-1], hs[i]); err != nil {
			return fmt.Errorf("header %d: %w", i, err)
		}
	}
	return nil
}

// Prefer reports whether a candidate chain of candidateLen blocks should
// replace the current chain of currentLen blocks: only if it is strictly
// longer. On a tie the chain already held is kept (first seen wins), so
// nodes never flip between equal chains.
func Prefer(candidateLen, currentLen uint64) bool {
	return candidateLen > currentLen
}
