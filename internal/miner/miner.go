// Package miner runs the parallel proof-of-work nonce search (DR-6).
//
// Several worker goroutines each claim a chunk of ChunkSize consecutive
// nonces from one atomic counter and search it in C++ (core.PowSearch). The
// counter hands every chunk to exactly one worker, so the ranges are
// disjoint and no worker idles while another still has work (dynamic
// partitioning). The first worker to find a solution wins a compare-and-swap
// and reports it; every worker stops at its next chunk boundary. Mine
// returns only after all workers have exited.
//
// A C++ search call cannot be interrupted, so cancellation (a stop flag set
// by the winner, or the caller's context) takes effect between chunks:
// within about one chunk, ~50 ms at the measured ~1.4 M hashes/s per thread.
//
// The miner does not validate what it returns; the node runs
// consensus.CheckHeader on the solved header before appending it, which
// recomputes the hash independently.
package miner

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/Anon-HP/Aletheia/internal/core"
)

// ChunkSize is the number of nonces one worker searches per C++ call. It
// bounds the cancellation delay and makes the per-call cgo overhead
// negligible.
const ChunkSize = 1 << 16

// Result is a solution: the nonce and the block hash it gives.
type Result struct {
	Nonce uint64
	Hash  core.Hash
}

// searchFunc is core.PowSearch's signature; tests substitute instrumented
// versions to observe chunk allocation and coordination.
type searchFunc func(h core.Header, start, count uint64, d uint32) (uint64, core.Hash, bool, error)

// Mine searches for a nonce that gives hdr (hdr.Nonce is ignored) a block
// hash with at least d leading zero bits, using workers goroutines
// (runtime.NumCPU() if workers < 1). Nonces are tried from 0 upwards in
// chunks. It returns the first solution found, or ctx.Err() if ctx is done
// first, or ErrArg if d exceeds core.MaxDifficulty. No goroutine outlives
// the call.
//
// The nonce counter would wrap only after 2^48 chunks (2^64 nonces), which
// is unreachable at any usable difficulty; it is not handled further.
func Mine(ctx context.Context, hdr core.Header, d uint32, workers int) (Result, error) {
	return mine(ctx, hdr, d, workers, core.PowSearch)
}

func mine(ctx context.Context, hdr core.Header, d uint32, workers int, search searchFunc) (Result, error) {
	if d > core.MaxDifficulty {
		return Result{}, core.ErrArg
	}
	if workers < 1 {
		workers = runtime.NumCPU()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	type outcome struct {
		r   Result
		err error
	}
	var (
		next   atomic.Uint64 // first nonce of the next unclaimed chunk
		stop   atomic.Bool   // set by the winner, and by Mine before waiting
		won    atomic.Bool   // the compare-and-swap that picks one winner
		result = make(chan outcome, 1)
		wg     sync.WaitGroup
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() && ctx.Err() == nil {
				// Fetch-and-add: this worker owns [start, start+ChunkSize).
				start := next.Add(ChunkSize) - ChunkSize
				n, h, found, err := search(hdr, start, ChunkSize, d)
				if !found && err == nil {
					continue
				}
				// Exactly one worker sends; the buffer of 1 means it never
				// blocks, and a simultaneous second solution is dropped.
				if won.CompareAndSwap(false, true) {
					result <- outcome{Result{Nonce: n, Hash: h}, err}
				}
				stop.Store(true)
				return
			}
		}()
	}

	var out outcome
	select {
	case out = <-result:
	case <-ctx.Done():
		out.err = ctx.Err()
	}
	stop.Store(true)
	wg.Wait()
	return out.r, out.err
}
