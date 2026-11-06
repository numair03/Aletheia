package core

/*
#include "pow_c.h"
*/
import "C"

// MaxDifficulty is the largest meaningful difficulty: a hash has 256 bits.
const MaxDifficulty = C.AL_POW_MAX_DIFFICULTY

// PowSearch searches the nonces start, start+1, ..., start+count-1 of header h
// (h.Nonce is ignored) for the first whose block hash has at least d leading
// zero bits. The range never wraps past 2^64-1. It returns that nonce and
// the block hash, or found == false if no nonce in the range qualifies.
//
// One call is one bounded chunk of work in C++: it cannot be cancelled, so
// callers keep count small (internal/miner uses 65,536). The C++ search is a
// pure function, so concurrent calls need no lock. The header is encoded by
// the C++ core (EncodeHeader); Go never lays out the bytes.
//
// Errors: ErrArg if d > MaxDifficulty.
func PowSearch(h Header, start, count uint64, d uint32) (nonce uint64, hash Hash, found bool, err error) {
	enc := EncodeHeader(h)
	var n C.uint64_t
	st := C.al_pow_search((*C.uint8_t)(&enc[0]), C.uint64_t(start), C.uint64_t(count),
		C.uint32_t(d), &n, (*C.uint8_t)(&hash[0]))
	switch st {
	case 1:
		return uint64(n), hash, true, nil
	case 0:
		return 0, Hash{}, false, nil
	}
	return 0, Hash{}, false, statusErr(st)
}

// PowCheck reports whether hash has at least d leading zero bits. A
// difficulty above MaxDifficulty can never be met, so it reports false.
// This is the proof-of-work predicate that consensus.CheckHeader applies to
// every block hash.
func PowCheck(hash Hash, d uint32) bool {
	return C.al_pow_check((*C.uint8_t)(&hash[0]), C.uint32_t(d)) == 1
}
