#ifndef ALETHEIA_MEMPOOL_C_H
#define ALETHEIA_MEMPOOL_C_H

/*
 * C interface to the fee-priority queue (aletheia::FeePriorityQueue), for cgo.
 *
 * Order: highest fee first; equal fees by ascending txid (bytes compared
 * lexicographically). The queue is a multiset: admission (balance, sequence
 * and duplicate checks) is the caller's job.
 *
 * Conventions (see al_status.h): int32_t status unless noted; caller-owned
 * buffers; the queue copies every transaction it stores and keeps no pointer
 * to caller memory. A queue handle is not thread-safe: the caller must
 * serialise every call on one handle.
 */

#include <stdint.h>

#include "al_status.h"
#include "ledger_c.h" /* al_tx */

#ifdef __cplusplus
extern "C" {
#endif

typedef struct al_feeq al_feeq;

/* Creates an empty queue. On failure *out is set to NULL. */
int32_t al_feeq_new(al_feeq** out);
void al_feeq_free(al_feeq* q); /* NULL is ignored */

/* Inserts a copy of *tx; its txid is computed inside with the canonical
 * encoding (the same txid as al_tx_id). */
int32_t al_feeq_push(al_feeq* q, const al_tx* tx);

/* Removes the highest-priority transaction into *out.
 * AL_ERR_NOT_FOUND (and *out untouched) if the queue is empty. */
int32_t al_feeq_pop(al_feeq* q, al_tx* out);

/* Like al_feeq_pop but leaves the transaction in the queue. */
int32_t al_feeq_peek(const al_feeq* q, al_tx* out);

/* Number of queued transactions. Returns 0 for NULL. */
uint64_t al_feeq_size(const al_feeq* q);

/* Removes every queued transaction whose txid is one of the n 32-byte txids
 * at `txids` (n * 32 contiguous bytes; repeats and unknown txids are
 * harmless). `txids` may be NULL only when n == 0. Compare al_feeq_size
 * before and after to learn how many were removed. */
int32_t al_feeq_remove_txids(al_feeq* q, const uint8_t* txids, uint32_t n);

#ifdef __cplusplus
}
#endif

#endif /* ALETHEIA_MEMPOOL_C_H */
