package core

/*
#include "mempool_c.h"
*/
import "C"

import (
	"errors"
	"math"
	"sync"
)

// ErrQueueClosed is returned by every FeeQueue method after Close.
var ErrQueueClosed = errors.New("core: fee queue is closed")

// FeeQueue is a handle to the C++ fee-priority queue (a binary max-heap).
// Pop returns the highest fee first; equal fees come out in ascending txid
// order, so every node pops the same sequence from the same contents.
//
// The queue is a multiset: it stores whatever it is given, duplicates
// included. Admission (balance, sequence and duplicate checks) belongs to the
// caller, internal/mempool.
//
// The C++ queue is not thread-safe, and Go's race detector cannot see inside
// C++, so every call holds mu for the whole C call; a FeeQueue is therefore
// safe for concurrent use. Callers that need several calls to act as one
// step (the mempool's admission, batch assembly, reconciliation) still need
// a lock of their own around them.
//
// Lifetime: the C++ memory is freed only by Close; there is no finalizer.
// After Close the handle is nil and every method returns ErrQueueClosed
// (Len returns 0).
type FeeQueue struct {
	mu sync.Mutex
	h  *C.al_feeq // nil once closed
}

// NewFeeQueue creates an empty queue.
func NewFeeQueue() (*FeeQueue, error) {
	var h *C.al_feeq
	if err := statusErr(C.al_feeq_new(&h)); err != nil {
		return nil, err
	}
	return &FeeQueue{h: h}, nil
}

// Close frees the C++ queue. Further calls, including a second Close,
// return ErrQueueClosed.
func (q *FeeQueue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.h == nil {
		return ErrQueueClosed
	}
	C.al_feeq_free(q.h)
	q.h = nil
	return nil
}

// with runs f with the handle under the lock, or returns ErrQueueClosed.
func (q *FeeQueue) with(f func(h *C.al_feeq) error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.h == nil {
		return ErrQueueClosed
	}
	return f(q.h)
}

// Push inserts a copy of tx in O(log n). Its txid is computed inside C++
// with the canonical encoding (the same value as TxID).
func (q *FeeQueue) Push(tx Tx) error {
	c := tx.toC()
	return q.with(func(h *C.al_feeq) error {
		return statusErr(C.al_feeq_push(h, &c))
	})
}

// Pop removes and returns the highest-priority transaction in O(log n);
// ErrNotFound if the queue is empty.
func (q *FeeQueue) Pop() (Tx, error) {
	var c C.al_tx
	if err := q.with(func(h *C.al_feeq) error {
		return statusErr(C.al_feeq_pop(h, &c))
	}); err != nil {
		return Tx{}, err
	}
	return txFromC(&c), nil
}

// Peek returns the highest-priority transaction without removing it, in
// O(1); ErrNotFound if the queue is empty.
func (q *FeeQueue) Peek() (Tx, error) {
	var c C.al_tx
	if err := q.with(func(h *C.al_feeq) error {
		return statusErr(C.al_feeq_peek(h, &c))
	}); err != nil {
		return Tx{}, err
	}
	return txFromC(&c), nil
}

// Len returns the number of queued transactions in O(1) (0 after Close).
func (q *FeeQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return int(C.al_feeq_size(q.h)) // al_feeq_size(NULL) is 0
}

// RemoveTxIDs removes every queued transaction whose txid is in ids and
// returns how many were removed. Unknown and repeated ids are harmless; all
// copies of a duplicated transaction are removed. C++ filters the heap
// through a hash set and rebuilds it bottom-up (Floyd), O(n + k) for n queued
// and k ids, rather than k separate deletions.
func (q *FeeQueue) RemoveTxIDs(ids []Hash) (int, error) {
	if uint64(len(ids)) > math.MaxUint32 {
		return 0, ErrArg // a uint32 count would silently truncate the list
	}
	removed := 0
	err := q.with(func(h *C.al_feeq) error {
		before := C.al_feeq_size(h)
		if err := statusErr(C.al_feeq_remove_txids(h, hashesPtr(ids), C.uint32_t(len(ids)))); err != nil {
			return err
		}
		removed = int(before - C.al_feeq_size(h)) // both sizes under one lock
		return nil
	})
	return removed, err
}
