// Package mempool is the node's pool of pending transactions: the C++
// fee-priority queue (core.FeeQueue) made safe for concurrent client
// handlers (Submit) and miners (WaitNonEmpty, Batch) by one Go mutex and a
// condition variable, plus admission checks and reconciliation with the
// chain.
//
// Admission policy: at most one pending transaction per sender. The ledger
// checks a transaction against committed state only, so a sender's next
// transaction (seq+1) is not admissible until the pending one (seq) is in a
// block; and two different transactions with the same sender and seq, or the
// same transaction twice, would both pass that check. Keeping one per sender
// rejects all three cases at the door.
//
// Lock order: Pool.mu, then the ledger's and the queue's own locks inside
// their methods. Neither the ledger nor the queue ever calls back into the
// pool, so the order cannot invert.
package mempool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/Anon-HP/Aletheia/internal/core"
)

var (
	// ErrDuplicate: the same transaction (same txid) is already pending.
	ErrDuplicate = errors.New("mempool: transaction already pending")
	// ErrSenderPending: the sender already has a different pending transaction.
	ErrSenderPending = errors.New("mempool: sender already has a pending transaction")
	// ErrClosed is returned by every method after Close.
	ErrClosed = errors.New("mempool: closed")
)

type entry struct {
	tx core.Tx
	id core.Hash
}

// Pool is a fee-ordered pool of pending transactions, safe for concurrent
// use. Invariant, whenever mu is free: every pending transaction is in the
// queue exactly once and in pending under its sender, and size equals both
// counts.
type Pool struct {
	mu       sync.Mutex
	nonEmpty *sync.Cond // signalled, with mu held, when a transaction is admitted or the pool closes

	q       *core.FeeQueue
	ledger  *core.Ledger        // admission and batches are checked against this ledger's committed state
	pending map[core.Hash]entry // sender → its pending transaction (pool bookkeeping, not world state)
	size    atomic.Int64        // written under mu; read without it by Len
	closed  bool
}

// New creates an empty pool that admits transactions against l. The pool
// does not own l and never closes it.
func New(l *core.Ledger) (*Pool, error) {
	if l == nil {
		return nil, core.ErrArg
	}
	q, err := core.NewFeeQueue()
	if err != nil {
		return nil, err
	}
	p := &Pool{q: q, ledger: l, pending: map[core.Hash]entry{}}
	p.nonEmpty = sync.NewCond(&p.mu)
	return p, nil
}

// Close frees the queue and wakes every WaitNonEmpty caller. Further calls,
// including a second Close, return ErrClosed.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	p.closed = true
	p.pending = nil
	p.size.Store(0)
	p.nonEmpty.Broadcast()
	return p.q.Close()
}

// Len is the number of pending transactions. O(1) and lock-free, so a status
// request never waits behind batch assembly (informational mempool size).
func (p *Pool) Len() int { return int(p.size.Load()) }

// Submit admits tx and returns its txid, or returns the reason it is
// rejected: ErrDuplicate, ErrSenderPending, a ledger rule error from
// core.Ledger.CheckTx (ErrBalance, ErrSeq, ErrOverflow), or ErrClosed.
func (p *Pool) Submit(tx core.Tx) (core.Hash, error) {
	id := core.TxID(tx) // pure C++ call; no lock needed
	p.mu.Lock()
	defer p.mu.Unlock()
	return id, p.admit(tx, id)
}

// admit runs the admission checks and enqueues tx. Caller holds mu.
func (p *Pool) admit(tx core.Tx, id core.Hash) error {
	if p.closed {
		return ErrClosed
	}
	if e, ok := p.pending[tx.Sender]; ok {
		if e.id == id {
			return ErrDuplicate
		}
		return ErrSenderPending
	}
	if err := p.ledger.CheckTx(tx); err != nil {
		return err
	}
	if err := p.q.Push(tx); err != nil {
		return err
	}
	p.pending[tx.Sender] = entry{tx, id}
	p.size.Add(1)
	p.nonEmpty.Broadcast()
	return nil
}

// WaitNonEmpty blocks until at least one transaction is pending (nil), ctx
// is done (ctx.Err()), or the pool is closed (ErrClosed). Miners call it so
// they never mine an empty block.
func (p *Pool) WaitNonEmpty(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// sync.Cond cannot wait on a channel, so cancellation is turned into a
	// Broadcast. It takes mu first, so it cannot fire between our ctx.Err
	// check and Wait (Wait releases mu atomically): no lost wakeup.
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.nonEmpty.Broadcast()
		p.mu.Unlock()
	})
	defer stop()
	for {
		if p.closed {
			return ErrClosed
		}
		if p.size.Load() > 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		p.nonEmpty.Wait()
	}
}

// walk pops transactions in fee order while visit returns true, then pushes
// every popped one back, so the pool's contents are unchanged. The C queue
// has no iteration API; pop-and-restore is how the heap order is read.
// Caller holds mu. Cost O(m log n) for m popped.
func (p *Pool) walk(visit func(core.Tx) (bool, error)) error {
	var popped []core.Tx
	var err error
	for {
		tx, perr := p.q.Pop()
		if errors.Is(perr, core.ErrNotFound) {
			break
		}
		if perr != nil {
			err = perr
			break
		}
		popped = append(popped, tx)
		more, verr := visit(tx)
		if verr != nil {
			err = verr
			break
		}
		if !more {
			break
		}
	}
	for _, tx := range popped {
		if perr := p.q.Push(tx); perr != nil && err == nil {
			err = perr
		}
	}
	return err
}

// Batch returns up to max pending transactions for the next block, in fee
// order, chosen greedily: each candidate is kept only if the kept list plus
// it still passes core.Ledger.CheckTxs (DECISIONS DR-12). The result is
// therefore a block body that the ledger will accept while its tip is
// unchanged. The transactions stay pending: they leave the pool only through
// Reconcile, once a block containing them is appended, so an abandoned
// mining attempt needs no undo.
func (p *Pool) Batch(max int) ([]core.Tx, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	var kept []core.Tx
	if max <= 0 {
		return nil, nil
	}
	err := p.walk(func(tx core.Tx) (bool, error) {
		cand := append(kept, tx) // a rejected tx in a spare slot is overwritten later
		if _, err := p.ledger.CheckTxs(cand); err != nil {
			if !isRule(err) {
				return false, err
			}
			return true, nil // conflicts with the kept list: skip it
		}
		kept = cand
		return len(kept) < max, nil
	})
	if err != nil {
		return nil, err
	}
	return kept, nil
}

// Pending returns every pending transaction in fee order.
func (p *Pool) Pending() ([]core.Tx, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	out := make([]core.Tx, 0, p.size.Load())
	err := p.walk(func(tx core.Tx) (bool, error) {
		out = append(out, tx)
		return true, nil
	})
	return out, err
}

// Reconcile brings the pool in line with the chain after a block was
// appended to l, or after the node switched to the replayed ledger l:
//  1. from now on, admission and batches are checked against l;
//  2. every pending transaction that l no longer accepts is evicted, in one
//     batch removal (hash-set filter + Floyd rebuild in C++, O(n + k)).
//     That covers the confirmed ones (their seq is used) and the stale ones
//     (conflicting with the new chain, or no longer affordable);
//  3. readd (the transactions of abandoned blocks after a chain switch, in
//     chain order) goes through normal admission; rejects are dropped.
//
// It returns how many were evicted and re-admitted. On an error from l
// (e.g. it is closed) nothing changes. The caller closes an old ledger only
// after Reconcile has switched the pool away from it.
func (p *Pool) Reconcile(l *core.Ledger, readd []core.Tx) (evicted, readmitted int, err error) {
	if l == nil {
		return 0, 0, core.ErrArg
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, 0, ErrClosed
	}
	var staleSenders []core.Hash
	var staleIDs []core.Hash
	for sender, e := range p.pending {
		if err := l.CheckTx(e.tx); err != nil {
			if !isRule(err) {
				return 0, 0, err
			}
			staleSenders = append(staleSenders, sender)
			staleIDs = append(staleIDs, e.id)
		}
	}
	p.ledger = l
	if len(staleIDs) > 0 {
		n, err := p.q.RemoveTxIDs(staleIDs)
		if err != nil {
			return 0, 0, err
		}
		for _, s := range staleSenders {
			delete(p.pending, s)
		}
		p.size.Add(-int64(n))
		evicted = n
	}
	for _, tx := range readd {
		if p.admit(tx, core.TxID(tx)) == nil {
			readmitted++
		}
	}
	return evicted, readmitted, nil
}

// isRule reports whether err is a ledger validation-rule rejection (as
// opposed to an infrastructure failure such as a closed ledger).
func isRule(err error) bool {
	for _, r := range []error{core.ErrLinkage, core.ErrMerkle, core.ErrTxSet,
		core.ErrBalance, core.ErrSeq, core.ErrOverflow} {
		if errors.Is(err, r) {
			return true
		}
	}
	return false
}
