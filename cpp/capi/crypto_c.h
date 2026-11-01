#ifndef ALETHEIA_CRYPTO_C_H
#define ALETHEIA_CRYPTO_C_H

/*
 * C API for SHA-256 and Merkle trees (implemented by aletheia::SHA256 and
 * aletheia::MerkleTree). Plain C types only, so it can be called from Go via cgo.
 *
 * Conventions:
 *   - Hashes are 32-byte arrays. A list of n hashes is n*32 contiguous bytes.
 *   - Output buffers are allocated by the caller; nothing returned here needs freeing.
 *   - Proof directions are bytes: 0 = sibling on the left, 1 = sibling on the right.
 *   - Status codes are defined in al_status.h.
 */

#include <stddef.h>
#include <stdint.h>

#include "al_status.h"

#ifdef __cplusplus
extern "C" {
#endif

/* Longest proof the API will produce or accept (a tree with up to 2^64 leaves). */
#define AL_MERKLE_MAX_STEPS 64

/* out = SHA-256(data[0..len)). `data` may be NULL only if len == 0. */
int32_t al_sha256(const uint8_t* data, size_t len, uint8_t out[32]);

/* Root of the Merkle tree over n leaves. n == 0 gives 32 zero bytes. */
int32_t al_merkle_root(const uint8_t* leaves, uint64_t n, uint8_t out[32]);

/*
 * Inclusion proof for leaves[index]. Writes the sibling hashes (steps*32 bytes)
 * and directions (steps bytes), leaf level first, and sets *out_steps.
 * If `cap` is smaller than the proof, returns AL_ERR_BUFFER and still sets
 * *out_steps to the required size. index >= n returns AL_ERR_ARG.
 */
int32_t al_merkle_proof(const uint8_t* leaves, uint64_t n, uint64_t index,
                        uint8_t* out_hashes, uint8_t* out_dirs, uint32_t cap,
                        uint32_t* out_steps);

/*
 * Returns 1 if folding the proof over `leaf` yields `root`, 0 if not (including
 * any direction byte other than 0 or 1), or a negative status on bad arguments.
 */
int32_t al_merkle_verify(const uint8_t root[32], const uint8_t leaf[32],
                         const uint8_t* hashes, const uint8_t* dirs, uint32_t steps);

#ifdef __cplusplus
}
#endif

#endif /* ALETHEIA_CRYPTO_C_H */
