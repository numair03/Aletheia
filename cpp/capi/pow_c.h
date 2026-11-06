#ifndef ALETHEIA_POW_C_H
#define ALETHEIA_POW_C_H

/*
 * C interface to the proof-of-work primitives (aletheia/pow.h), for cgo.
 *
 * A header meets difficulty d when SHA-256 of its canonical 88-byte encoding
 * starts with at least d zero bits (big-endian bit order). Both functions
 * are pure: no state, no allocation, safe to call from several threads at
 * once. Arguments are checked before any input is read.
 */

#include <stdint.h>

#include "al_status.h"

#ifdef __cplusplus
extern "C" {
#endif

#define AL_POW_MAX_DIFFICULTY 256

/* Tries the nonces start, start+1, ..., start+count-1 in order, each written
 * big-endian into bytes 80..87 of a copy of `header` (a canonical encoded
 * header; its own nonce bytes are ignored), and stops at the first whose
 * hash meets `difficulty`. The range never wraps past 2^64-1.
 *
 * Returns 1 if found (*out_nonce and out_hash set), 0 if no nonce in the
 * range meets the difficulty (outputs untouched), or AL_ERR_ARG for a NULL
 * pointer or difficulty > AL_POW_MAX_DIFFICULTY. */
int32_t al_pow_search(const uint8_t header[88], uint64_t start, uint64_t count,
                      uint32_t difficulty, uint64_t* out_nonce, uint8_t out_hash[32]);

/* Returns 1 if `hash` has at least `difficulty` leading zero bits, 0 if not,
 * or AL_ERR_ARG for a NULL hash or difficulty > AL_POW_MAX_DIFFICULTY. */
int32_t al_pow_check(const uint8_t hash[32], uint32_t difficulty);

#ifdef __cplusplus
}
#endif

#endif /* ALETHEIA_POW_C_H */
