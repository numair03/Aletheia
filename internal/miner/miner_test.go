package miner

import (
	"context"
	"errors"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anon-HP/Aletheia/internal/consensus"
	"github.com/Anon-HP/Aletheia/internal/core"
)

// waitGoroutines fails the test unless the goroutine count drops back to
// base: no worker may outlive Mine.
func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines, %d before Mine", runtime.NumGoroutine(), base)
		}
		time.Sleep(time.Millisecond)
	}
}

// candidate returns an unsolved successor of genesis at difficulty d.
func candidate(p consensus.Params, ts int64) core.Header {
	return core.Header{
		Version:    1,
		Prev:       core.HeaderHash(p.Genesis),
		MerkleRoot: core.SHA256([]byte("block")),
		Timestamp:  ts,
		Difficulty: p.Difficulty,
		Nonce:      12345, // ignored by Mine
	}
}

func lowParams(d uint32) consensus.Params {
	p := consensus.Default()
	p.Difficulty, p.Genesis.Difficulty = d, d
	return p
}

// The mined header passes the independent consensus check, and the reported
// hash is the header's real block hash.
func TestMineSolvesHeader(t *testing.T) {
	p := lowParams(16)
	for i := 0; i < 5; i++ {
		base := runtime.NumGoroutine()
		h := candidate(p, int64(i))
		r, err := Mine(context.Background(), h, p.Difficulty, 4)
		if err != nil {
			t.Fatal(err)
		}
		h.Nonce = r.Nonce
		if core.HeaderHash(h) != r.Hash {
			t.Fatal("reported hash is not the solved header's hash")
		}
		if err := consensus.CheckHeader(p, p.Genesis, h); err != nil {
			t.Fatalf("mined header rejected: %v", err)
		}
		waitGoroutines(t, base)
	}
}

// With one worker the chunks are searched in order, so Mine returns the
// smallest solution, which a plain sequential scan also finds.
func TestMineOneWorkerFindsFirst(t *testing.T) {
	p := lowParams(15)
	for i := 0; i < 3; i++ {
		h := candidate(p, 100+int64(i))
		want, _, found, _ := core.PowSearch(h, 0, 64*ChunkSize, p.Difficulty)
		if !found {
			t.Fatal("no solution in 64 chunks")
		}
		r, err := Mine(context.Background(), h, p.Difficulty, 1)
		if err != nil || r.Nonce != want {
			t.Fatalf("got %d (%v), want %d", r.Nonce, err, want)
		}
	}
}

// recorder is an instrumented search that logs every chunk it is given.
type recorder struct {
	mu     sync.Mutex
	starts []uint64
	counts []uint64
	calls  atomic.Int64
}

func (rec *recorder) log(start, count uint64) {
	rec.calls.Add(1)
	rec.mu.Lock()
	rec.starts = append(rec.starts, start)
	rec.counts = append(rec.counts, count)
	rec.mu.Unlock()
}

// Chunks are disjoint, ChunkSize long, aligned, and contiguous from 0: every
// claimed chunk is searched, none twice, none skipped.
func TestChunkAllocation(t *testing.T) {
	for _, workers := range []int{1, 3, 8, 32} {
		target := uint64(37*ChunkSize + 123)
		rec := &recorder{}
		search := func(_ core.Header, start, count uint64, _ uint32) (uint64, core.Hash, bool, error) {
			rec.log(start, count)
			time.Sleep(50 * time.Microsecond) // let workers interleave
			if target >= start && target-start < count {
				return target, core.Hash{1}, true, nil
			}
			return 0, core.Hash{}, false, nil
		}
		base := runtime.NumGoroutine()
		r, err := mine(context.Background(), core.Header{}, 20, workers, search)
		if err != nil || r.Nonce != target || r.Hash != (core.Hash{1}) {
			t.Fatalf("workers %d: %+v, %v", workers, r, err)
		}
		waitGoroutines(t, base)
		after := rec.calls.Load()

		sort.Slice(rec.starts, func(i, j int) bool { return rec.starts[i] < rec.starts[j] })
		for i, s := range rec.starts {
			if s != uint64(i)*ChunkSize {
				t.Fatalf("workers %d: chunk %d starts at %d", workers, i, s)
			}
			if rec.counts[i] != ChunkSize {
				t.Fatalf("workers %d: chunk of %d nonces", workers, rec.counts[i])
			}
		}
		if len(rec.starts) < 38 {
			t.Fatalf("workers %d: only %d chunks searched", workers, len(rec.starts))
		}
		// One worker searches chunks strictly in order and stops at the
		// winner; with more, only the chunks in flight when the winner is
		// found are added (a loose bound, to stay robust under scheduling).
		if (workers == 1 && len(rec.starts) != 38) || len(rec.starts) > 38+4*workers {
			t.Fatalf("workers %d: %d chunks searched", workers, len(rec.starts))
		}
		time.Sleep(20 * time.Millisecond)
		if rec.calls.Load() != after {
			t.Fatalf("workers %d: search called after Mine returned", workers)
		}
	}
}

// Difficulty 0: every worker finds a solution in its first chunk at almost
// the same moment. Exactly one is reported; the others are dropped without
// blocking (a second send would deadlock wg.Wait and hang this test).
func TestDenseSolutionsOneWinner(t *testing.T) {
	p := lowParams(0)
	for i := 0; i < 200; i++ {
		var finds atomic.Int64
		search := func(h core.Header, start, count uint64, d uint32) (uint64, core.Hash, bool, error) {
			n, hash, found, err := core.PowSearch(h, start, count, d)
			if found {
				finds.Add(1)
			}
			return n, hash, found, err
		}
		base := runtime.NumGoroutine()
		h := candidate(p, int64(i))
		r, err := mine(context.Background(), h, 0, 16, search)
		if err != nil {
			t.Fatal(err)
		}
		if r.Nonce%ChunkSize != 0 || finds.Load() < 1 {
			t.Fatalf("nonce %d is not a chunk start (finds %d)", r.Nonce, finds.Load())
		}
		h.Nonce = r.Nonce
		if core.HeaderHash(h) != r.Hash || consensus.CheckHeader(p, p.Genesis, h) != nil {
			t.Fatal("reported result is not a valid solution")
		}
		waitGoroutines(t, base)
	}
}

// Cancelling the context stops a search that would never finish, within
// about one chunk, and leaves no goroutine behind.
func TestCancel(t *testing.T) {
	h := candidate(lowParams(0), 1)
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var cancelledAt time.Time
	go func() {
		_, err := Mine(ctx, h, 200, 0) // 200 bits: never found; 0 = NumCPU workers
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)
	cancelledAt = time.Now()
	cancel()
	select {
	case err := <-done:
		lat := time.Since(cancelledAt)
		t.Logf("returned %v after cancel (one chunk = %d nonces)", lat, ChunkSize)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v, want context.Canceled", err)
		}
		if lat > time.Second {
			t.Fatalf("returned %v after cancel", lat)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Mine did not return after cancel")
	}
	waitGoroutines(t, base)

	// A deadline ends the search the same way.
	dctx, dcancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer dcancel()
	if _, err := Mine(dctx, h, 200, 2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}

	// An already-cancelled context returns at once without searching.
	var calls atomic.Int64
	search := func(core.Header, uint64, uint64, uint32) (uint64, core.Hash, bool, error) {
		calls.Add(1)
		return 0, core.Hash{}, false, nil
	}
	if _, err := mine(ctx, h, 10, 4, search); !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("pre-cancelled: %v, %d calls", err, calls.Load())
	}
	waitGoroutines(t, base)
}

func TestMineErrors(t *testing.T) {
	if _, err := Mine(context.Background(), core.Header{}, core.MaxDifficulty+1, 2); !errors.Is(err, core.ErrArg) {
		t.Fatalf("d=257: %v", err)
	}
	// A search error stops all workers and is returned.
	boom := errors.New("boom")
	search := func(_ core.Header, start, _ uint64, _ uint32) (uint64, core.Hash, bool, error) {
		if start == 5*ChunkSize {
			return 0, core.Hash{}, false, boom
		}
		return 0, core.Hash{}, false, nil
	}
	base := runtime.NumGoroutine()
	if _, err := mine(context.Background(), core.Header{}, 10, 4, search); !errors.Is(err, boom) {
		t.Fatalf("search error: %v", err)
	}
	waitGoroutines(t, base)
}
