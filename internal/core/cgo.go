// Package core is the Go binding of the C++ core library (cpp/lib/libaletheia_core.a):
// SHA-256, Merkle trees, the canonical transaction and header encodings, and
// the ledger. It is the only package in the module that imports "C".
//
// Build: the library must exist before this package is compiled. Use the
// Makefile (`make go-build`, `make go-test`), which builds the library first
// and passes a hash of it in CGO_LDFLAGS so that Go's build cache never links
// a stale copy (the Go cache does not track the .a file itself).
//
// Boundary rules: every C function returns an int32_t status and never lets a
// C++ exception escape; Go allocates every output buffer; C++ copies whatever
// it keeps, so no Go pointer is retained after a call returns; C++ never calls
// back into Go. C++ objects are not thread-safe, so every handle is guarded by
// a Go lock (see Ledger). Go's race detector cannot see races inside C++, so
// that lock is the only protection.
package core

/*
#cgo CFLAGS: -I${SRCDIR}/../../cpp/capi
#cgo LDFLAGS: -L${SRCDIR}/../../cpp/lib -laletheia_core -lstdc++
#include "al_status.h"
#include "ledger_c.h"
*/
import "C"

import "fmt"

// Error is a failure status returned by the C++ core. Each status code has
// exactly one Error value, so callers compare with errors.Is or ==.
type Error struct {
	Code int32
	msg  string
}

func (e *Error) Error() string { return "core: " + e.msg }

// Infrastructure errors (al_status.h).
var (
	ErrArg      = &Error{C.AL_ERR_ARG, "invalid argument"}
	ErrBuffer   = &Error{C.AL_ERR_BUFFER, "output buffer too small"}
	ErrNotFound = &Error{C.AL_ERR_NOT_FOUND, "not found"}
	ErrInternal = &Error{C.AL_ERR_INTERNAL, "internal C++ error"}
	ErrUnknown  = &Error{C.AL_ERR_UNKNOWN, "unknown C++ exception"}
)

// Validation-rule errors (ledger_c.h): why a block or transaction was rejected.
var (
	ErrLinkage  = &Error{C.AL_ERR_LINKAGE, "prev is not the hash of the tip"}
	ErrMerkle   = &Error{C.AL_ERR_MERKLE, "merkle root does not match the transactions"}
	ErrTxSet    = &Error{C.AL_ERR_TXSET, "empty transaction set or duplicate txid"}
	ErrBalance  = &Error{C.AL_ERR_BALANCE, "sender cannot pay amount + fee"}
	ErrSeq      = &Error{C.AL_ERR_SEQ, "wrong sequence number"}
	ErrOverflow = &Error{C.AL_ERR_OVERFLOW, "amount overflow"}
)

var statusErrors = []*Error{
	ErrArg, ErrBuffer, ErrNotFound, ErrInternal, ErrUnknown,
	ErrLinkage, ErrMerkle, ErrTxSet, ErrBalance, ErrSeq, ErrOverflow,
}

// statusErr converts a C status into nil (AL_OK) or the matching Error.
func statusErr(st C.int32_t) error { return codeErr(int32(st)) }

// codeErr maps a status code to nil or its Error. A code the binding does not
// know becomes a new Error that keeps the code.
func codeErr(code int32) error {
	if code == C.AL_OK {
		return nil
	}
	for _, e := range statusErrors {
		if e.Code == code {
			return e
		}
	}
	return &Error{code, fmt.Sprintf("unexpected status %d", code)}
}

// must is for calls that can fail only on arguments this package never passes
// (null pointers): a non-OK status there is a bug in the binding or the
// library, not a runtime condition, so it panics instead of returning an error.
func must(st C.int32_t) {
	if err := statusErr(st); err != nil {
		panic(err)
	}
}
