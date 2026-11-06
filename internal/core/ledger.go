package core

/*
#include "ledger_c.h"
*/
import "C"

import (
	"errors"
	"math"
	"sync"
)

// ErrClosed is returned by every Ledger method after Close.
var ErrClosed = errors.New("core: ledger is closed")

// Ledger is a handle to a C++ ledger: the hash-linked chain plus the world
// state it produces. It is safe for concurrent use.
//
// The C++ ledger is not thread-safe, and Go's race detector cannot see inside
// C++, so this lock is the only protection: Append and Close hold it
// exclusively; every other method holds it shared. Concurrent readers are
// sound because the C++ read paths are const and touch no shared mutable
// state. The lock is held for the whole C call and never across two calls
// that a caller makes separately, so methods that need a consistent view
// (Tip, BlockTxs) do all their C calls under one acquisition.
//
// Lifetime: the C++ memory is freed only by Close; there is no finalizer.
// After Close the handle is nil and every method returns ErrClosed, so a
// goroutine still holding the *Ledger (for example after the node swapped in
// a replayed ledger) gets an error, never a use-after-free.
type Ledger struct {
	mu sync.RWMutex
	h  *C.al_ledger // nil once closed
}

// NewLedger creates a ledger holding only genesis, with the given balances.
// ErrArg if genesis.Prev or genesis.MerkleRoot is not zero, or an account is
// listed twice.
func NewLedger(genesis Header, allocs []Alloc) (*Ledger, error) {
	if uint64(len(allocs)) > math.MaxUint32 {
		return nil, ErrArg
	}
	g := genesis.toC()
	ca := make([]C.al_alloc, len(allocs))
	for i, a := range allocs {
		ca[i] = C.al_alloc{account: hashToC(a.Account), balance: C.uint64_t(a.Balance)}
	}
	var pa *C.al_alloc
	if len(ca) > 0 {
		pa = &ca[0]
	}
	var h *C.al_ledger
	if err := statusErr(C.al_ledger_new(&g, pa, C.uint32_t(len(ca)), &h)); err != nil {
		return nil, err
	}
	return &Ledger{h: h}, nil
}

// Close frees the C++ ledger. Further calls, including a second Close,
// return ErrClosed. Close waits for calls already in progress to finish.
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.h == nil {
		return ErrClosed
	}
	C.al_ledger_free(l.h)
	l.h = nil
	return nil
}

// read runs f with the handle under the shared lock, or returns ErrClosed.
func (l *Ledger) read(f func(h *C.al_ledger) error) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.h == nil {
		return ErrClosed
	}
	return f(l.h)
}

// Height is the tip's height (genesis = 0).
func (l *Ledger) Height() (uint64, error) {
	var n uint64
	err := l.read(func(h *C.al_ledger) error {
		n = uint64(C.al_ledger_height(h))
		return nil
	})
	return n, err
}

// HeaderAt returns the header at height; ErrNotFound beyond the tip.
func (l *Ledger) HeaderAt(height uint64) (Header, error) {
	var c C.al_header
	err := l.read(func(h *C.al_ledger) error {
		return statusErr(C.al_ledger_header_at(h, C.uint64_t(height), &c))
	})
	if err != nil {
		return Header{}, err
	}
	return headerFromC(&c), nil
}

// Tip returns the tip's height and header as one consistent snapshot (two
// separate calls could straddle an Append).
func (l *Ledger) Tip() (uint64, Header, error) {
	var n uint64
	var c C.al_header
	err := l.read(func(h *C.al_ledger) error {
		n = uint64(C.al_ledger_height(h))
		return statusErr(C.al_ledger_header_at(h, C.uint64_t(n), &c))
	})
	if err != nil {
		return 0, Header{}, err
	}
	return n, headerFromC(&c), nil
}

// BlockTxs returns the transactions of the block at height, in order
// (none for genesis); ErrNotFound beyond the tip.
func (l *Ledger) BlockTxs(height uint64) ([]Tx, error) {
	var out []Tx
	err := l.read(func(h *C.al_ledger) error {
		// First ask for the count (cap 0), then fetch into a buffer of that size.
		var n C.uint32_t
		st := C.al_ledger_block_txs(h, C.uint64_t(height), nil, 0, &n)
		if err := statusErr(st); err != nil && err != ErrBuffer {
			return err
		}
		if n == 0 {
			return nil
		}
		buf := make([]C.al_tx, n)
		if err := statusErr(C.al_ledger_block_txs(h, C.uint64_t(height), &buf[0], n, &n)); err != nil {
			return err
		}
		out = make([]Tx, n)
		for i := range out {
			out[i] = txFromC(&buf[i])
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Account returns an account's committed balance and next sequence number.
// An account never seen has balance 0 and next sequence 0.
func (l *Ledger) Account(id Hash) (balance, nextSeq uint64, err error) {
	var b, s C.uint64_t
	err = l.read(func(h *C.al_ledger) error {
		return statusErr(C.al_ledger_account(h, (*C.uint8_t)(&id[0]), &b, &s))
	})
	return uint64(b), uint64(s), err
}

// CheckTx reports whether tx is valid as its sender's next transaction
// against the committed state (mempool admission): nil, ErrBalance, ErrSeq or
// ErrOverflow. It does not see other pending transactions.
func (l *Ledger) CheckTx(tx Tx) error {
	c := tx.toC()
	return l.read(func(h *C.al_ledger) error {
		return statusErr(C.al_ledger_check_tx(h, &c))
	})
}

// CheckTxs reports whether txs, applied in this order, would be valid as the
// transactions of the next block. On a rule failure it returns the error of
// the first failing transaction and that transaction's index; on success,
// -1 and nil. An empty list returns nil (Append still rejects an empty block).
// Block assembly uses this, because transactions that each passed CheckTx
// can conflict with one another (DECISIONS DR-12).
func (l *Ledger) CheckTxs(txs []Tx) (int, error) {
	if uint64(len(txs)) > math.MaxUint32 {
		return -1, ErrArg
	}
	c := txsToC(txs)
	bad := -1
	err := l.read(func(h *C.al_ledger) error {
		var idx C.uint32_t
		err := statusErr(C.al_ledger_check_txs(h, txsPtr(c), C.uint32_t(len(c)), &idx))
		var ce *Error
		if errors.As(err, &ce) && ce.Code <= C.AL_ERR_LINKAGE && ce.Code >= C.AL_ERR_OVERFLOW {
			bad = int(idx) // set by C only for rule failures
		}
		return err
	})
	return bad, err
}

// Append validates b as the new tip and appends it atomically, or returns
// the first rule it breaks (ErrLinkage .. ErrOverflow) and changes nothing.
// Proof of work is not checked here: the caller (internal/node) runs
// consensus.CheckHeader first.
func (l *Ledger) Append(b Block) error {
	if uint64(len(b.Txs)) > math.MaxUint32 {
		return ErrArg // a uint32 count would silently truncate the block
	}
	hc := b.Header.toC()
	c := txsToC(b.Txs)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.h == nil {
		return ErrClosed
	}
	return statusErr(C.al_ledger_append(l.h, &hc, txsPtr(c), C.uint32_t(len(c))))
}

// FindTx returns the height and index of a confirmed transaction;
// ErrNotFound if it is not in the chain.
func (l *Ledger) FindTx(txid Hash) (height uint64, index uint32, err error) {
	var hh C.uint64_t
	var ix C.uint32_t
	err = l.read(func(h *C.al_ledger) error {
		return statusErr(C.al_ledger_find_tx(h, (*C.uint8_t)(&txid[0]), &hh, &ix))
	})
	return uint64(hh), uint32(ix), err
}
