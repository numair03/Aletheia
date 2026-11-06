package core

import (
	"bytes"
	"errors"
	"math"
	"math/rand"
	"sort"
	"sync"
	"testing"
)

func newTestQueue(t *testing.T) *FeeQueue {
	t.Helper()
	q, err := NewFeeQueue()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	return q
}

// before is the queue's priority: higher fee first, then smaller txid.
func before(a, b Tx) bool {
	if a.Fee != b.Fee {
		return a.Fee > b.Fee
	}
	ia, ib := TxID(a), TxID(b)
	return bytes.Compare(ia[:], ib[:]) < 0
}

func drainQueue(t *testing.T, q *FeeQueue) []Tx {
	t.Helper()
	var out []Tx
	for {
		tx, err := q.Pop()
		if errors.Is(err, ErrNotFound) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tx)
	}
}

func TestFeeQueueEmpty(t *testing.T) {
	q := newTestQueue(t)
	if q.Len() != 0 {
		t.Fatalf("Len = %d", q.Len())
	}
	if _, err := q.Pop(); err != ErrNotFound {
		t.Fatalf("Pop on empty: %v", err)
	}
	if _, err := q.Peek(); err != ErrNotFound {
		t.Fatalf("Peek on empty: %v", err)
	}
	if n, err := q.RemoveTxIDs(nil); n != 0 || err != nil {
		t.Fatalf("RemoveTxIDs(nil) = %d, %v", n, err)
	}
	if n, err := q.RemoveTxIDs([]Hash{acct(9)}); n != 0 || err != nil {
		t.Fatalf("RemoveTxIDs on empty = %d, %v", n, err)
	}
}

func TestFeeQueueOrder(t *testing.T) {
	q := newTestQueue(t)
	fees := []uint64{4, math.MaxUint64, 0, 9, 4, 7}
	var in []Tx
	for i, f := range fees {
		tx := pay(acct(byte(i+1)), bob, 10, f, uint64(i))
		in = append(in, tx)
		if err := q.Push(tx); err != nil {
			t.Fatal(err)
		}
	}
	if q.Len() != len(fees) {
		t.Fatalf("Len = %d", q.Len())
	}
	want := append([]Tx(nil), in...)
	sort.Slice(want, func(i, j int) bool { return before(want[i], want[j]) })

	top, err := q.Peek()
	if err != nil || top != want[0] {
		t.Fatalf("Peek = %+v, %v; want %+v", top, err, want[0])
	}
	if q.Len() != len(fees) {
		t.Fatal("Peek removed an entry")
	}
	got := drainQueue(t, q)
	if len(got) != len(want) {
		t.Fatalf("drained %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] { // every field survives the round trip
			t.Fatalf("pop %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Equal fees come out in ascending txid order, whatever the push order.
func TestFeeQueueTieBreak(t *testing.T) {
	var txs []Tx
	for i := 0; i < 8; i++ {
		txs = append(txs, pay(acct(byte(i+1)), carol, 1, 5, 0))
	}
	want := append([]Tx(nil), txs...)
	sort.Slice(want, func(i, j int) bool {
		a, b := TxID(want[i]), TxID(want[j])
		return bytes.Compare(a[:], b[:]) < 0
	})
	for _, reverse := range []bool{false, true} {
		q := newTestQueue(t)
		for i := range txs {
			k := i
			if reverse {
				k = len(txs) - 1 - i
			}
			q.Push(txs[k])
		}
		got := drainQueue(t, q)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("reverse=%v: pop %d has txid %s, want %s", reverse, i, TxID(got[i]), TxID(want[i]))
			}
		}
	}
	// A higher fee beats any txid.
	q := newTestQueue(t)
	for _, tx := range txs {
		q.Push(tx)
	}
	hi := pay(acct(99), carol, 1, 6, 0)
	q.Push(hi)
	if top, _ := q.Peek(); top != hi {
		t.Fatalf("top = %+v, want the fee-6 transaction", top)
	}
}

func TestFeeQueueRemove(t *testing.T) {
	q := newTestQueue(t)
	var txs []Tx
	for i := 0; i < 30; i++ {
		tx := pay(acct(byte(i+1)), bob, 1, uint64(i%7), 0)
		txs = append(txs, tx)
		q.Push(tx)
	}
	dup := txs[4]
	q.Push(dup) // the queue is a multiset: a second copy

	// Every third transaction, the duplicate's txid twice, and an unknown id.
	var ids []Hash
	gone := map[Hash]bool{}
	for i := 0; i < len(txs); i += 3 {
		ids = append(ids, TxID(txs[i]))
		gone[TxID(txs[i])] = true
	}
	ids = append(ids, TxID(dup), TxID(dup), acct(200))
	gone[TxID(dup)] = true

	n, err := q.RemoveTxIDs(ids)
	wantRemoved := 10 + 2 // indices 0,3,…,27 plus both copies of txs[4]
	if err != nil || n != wantRemoved {
		t.Fatalf("RemoveTxIDs = %d, %v; want %d", n, err, wantRemoved)
	}
	var want []Tx
	for _, tx := range txs {
		if !gone[TxID(tx)] {
			want = append(want, tx)
		}
	}
	sort.Slice(want, func(i, j int) bool { return before(want[i], want[j]) })
	if q.Len() != len(want) {
		t.Fatalf("Len = %d, want %d", q.Len(), len(want))
	}
	got := drainQueue(t, q)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("after removal, pop %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFeeQueueClose(t *testing.T) {
	q, err := NewFeeQueue()
	if err != nil {
		t.Fatal(err)
	}
	q.Push(pay(alice, bob, 1, 1, 0))
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(pay(alice, bob, 1, 1, 0)); err != ErrQueueClosed {
		t.Fatalf("Push: %v", err)
	}
	if _, err := q.Pop(); err != ErrQueueClosed {
		t.Fatalf("Pop: %v", err)
	}
	if _, err := q.Peek(); err != ErrQueueClosed {
		t.Fatalf("Peek: %v", err)
	}
	if _, err := q.RemoveTxIDs([]Hash{alice}); err != ErrQueueClosed {
		t.Fatalf("RemoveTxIDs: %v", err)
	}
	if q.Len() != 0 {
		t.Fatalf("Len after Close = %d", q.Len())
	}
	if err := q.Close(); err != ErrQueueClosed {
		t.Fatalf("second Close: %v", err)
	}
}

// Random push/pop/peek/remove against a sorted-slice model, checked after
// every operation.
func TestFeeQueueModel(t *testing.T) {
	q := newTestQueue(t)
	rng := rand.New(rand.NewSource(7))
	var model []Tx // kept sorted by priority
	for step := 0; step < 3000; step++ {
		switch op := rng.Intn(10); {
		case op < 5:
			var tx Tx
			if len(model) > 0 && rng.Intn(8) == 0 {
				tx = model[rng.Intn(len(model))] // exact duplicate
			} else {
				tx = pay(acct(byte(rng.Intn(256))), bob, uint64(rng.Intn(100)), uint64(rng.Intn(6)), uint64(rng.Intn(1000)))
			}
			if err := q.Push(tx); err != nil {
				t.Fatal(err)
			}
			i := sort.Search(len(model), func(i int) bool { return before(tx, model[i]) })
			model = append(model[:i], append([]Tx{tx}, model[i:]...)...)
		case op < 8:
			tx, err := q.Pop()
			if len(model) == 0 {
				if err != ErrNotFound {
					t.Fatalf("step %d: Pop on empty: %v", step, err)
				}
				continue
			}
			if err != nil || tx != model[0] {
				t.Fatalf("step %d: Pop = %+v, %v; want %+v", step, tx, err, model[0])
			}
			model = model[1:]
		case op < 9:
			tx, err := q.Peek()
			if len(model) > 0 && (err != nil || tx != model[0]) {
				t.Fatalf("step %d: Peek = %+v, %v", step, tx, err)
			}
		default:
			var ids []Hash
			drop := map[Hash]bool{}
			for k := rng.Intn(4); k > 0 && len(model) > 0; k-- {
				id := TxID(model[rng.Intn(len(model))])
				ids = append(ids, id)
				drop[id] = true
			}
			ids = append(ids, acct(byte(rng.Intn(256)))) // unknown id
			kept := model[:0:0]
			for _, tx := range model {
				if !drop[TxID(tx)] {
					kept = append(kept, tx)
				}
			}
			n, err := q.RemoveTxIDs(ids)
			if err != nil || n != len(model)-len(kept) {
				t.Fatalf("step %d: RemoveTxIDs = %d, %v; want %d", step, n, err, len(model)-len(kept))
			}
			model = kept
		}
		if q.Len() != len(model) {
			t.Fatalf("step %d: Len = %d, want %d", step, q.Len(), len(model))
		}
	}
	got := drainQueue(t, q)
	for i := range model {
		if got[i] != model[i] {
			t.Fatalf("final drain %d differs", i)
		}
	}
}

// Concurrent pushers and poppers on one handle: nothing lost, nothing
// duplicated. Run under -race; the C++ side is protected only by the lock.
func TestFeeQueueConcurrent(t *testing.T) {
	q := newTestQueue(t)
	const pushers, perPusher, poppers = 8, 300, 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[Hash]int{}
	done := make(chan struct{})

	for p := 0; p < pushers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perPusher; i++ {
				if err := q.Push(pay(acct(byte(p)), bob, 1, uint64(i%10), uint64(i))); err != nil {
					t.Error(err)
					return
				}
			}
		}(p)
	}
	var cwg sync.WaitGroup
	for c := 0; c < poppers; c++ {
		cwg.Add(1)
		go func() {
			defer cwg.Done()
			for {
				tx, err := q.Pop()
				if err == nil {
					mu.Lock()
					seen[TxID(tx)]++
					mu.Unlock()
					continue
				}
				if err != ErrNotFound {
					t.Error(err)
					return
				}
				select {
				case <-done:
					return
				default:
				}
				q.Len()
				q.Peek()
			}
		}()
	}
	wg.Wait()
	close(done)
	cwg.Wait()
	for _, tx := range drainQueue(t, q) {
		seen[TxID(tx)]++
	}
	if len(seen) != pushers*perPusher {
		t.Fatalf("saw %d distinct transactions, want %d", len(seen), pushers*perPusher)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("txid %s popped %d times", id, n)
		}
	}
}
