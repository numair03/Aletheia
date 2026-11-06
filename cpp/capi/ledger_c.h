#ifndef ALETHEIA_LEDGER_C_H
#define ALETHEIA_LEDGER_C_H

/*
 * C interface to transactions, headers and the ledger, for cgo.
 *
 * Conventions (see al_status.h): every function returns an int32_t status
 * unless noted; output buffers are allocated by the caller; nothing returned
 * points into C++-owned memory. A ledger handle is not thread-safe: the caller
 * must not use one handle from two threads at once.
 */

#include <stddef.h>
#include <stdint.h>

#include "al_status.h"

#ifdef __cplusplus
extern "C" {
#endif

/* Why al_ledger_append / al_ledger_check_tx rejected their input. */
#define AL_ERR_LINKAGE (-10)  /* prev is not the hash of the current tip */
#define AL_ERR_MERKLE (-11)   /* merkle_root does not match the transactions */
#define AL_ERR_TXSET (-12)    /* no transactions, or a duplicate txid */
#define AL_ERR_BALANCE (-13)  /* sender cannot pay amount + fee */
#define AL_ERR_SEQ (-14)      /* seq is not the sender's next sequence number */
#define AL_ERR_OVERFLOW (-15) /* an amount would exceed 2^64 - 1 */

#define AL_TX_SIZE 88
#define AL_HEADER_SIZE 88

typedef struct {
    uint8_t sender[32];
    uint8_t recipient[32];
    uint64_t amount;
    uint64_t fee;
    uint64_t seq;
} al_tx;

typedef struct {
    uint32_t version;
    uint8_t prev[32];
    uint8_t merkle_root[32];
    int64_t timestamp;
    uint32_t difficulty;
    uint64_t nonce;
} al_header;

typedef struct {
    uint8_t account[32];
    uint64_t balance;
} al_alloc;

typedef struct al_ledger al_ledger;

/* Canonical 88-byte encodings, the txid and the block hash. */
int32_t al_tx_encode(const al_tx* tx, uint8_t out[AL_TX_SIZE]);
int32_t al_tx_decode(const uint8_t in[AL_TX_SIZE], al_tx* out);
int32_t al_tx_id(const al_tx* tx, uint8_t out[32]);
int32_t al_header_encode(const al_header* h, uint8_t out[AL_HEADER_SIZE]);
int32_t al_header_decode(const uint8_t in[AL_HEADER_SIZE], al_header* out);
int32_t al_header_hash(const al_header* h, uint8_t out[32]);

/* Creates a ledger holding only `genesis`, with the given balances.
 * AL_ERR_ARG if genesis.prev or genesis.merkle_root is not all zero or an
 * account is listed twice. On failure *out is set to NULL. */
int32_t al_ledger_new(const al_header* genesis, const al_alloc* allocs, uint32_t n,
                      al_ledger** out);
void al_ledger_free(al_ledger* l); /* NULL is ignored */

/* Height of the tip (genesis = 0). Returns 0 for NULL. */
uint64_t al_ledger_height(const al_ledger* l);

/* AL_ERR_NOT_FOUND if height is beyond the tip. */
int32_t al_ledger_header_at(const al_ledger* l, uint64_t height, al_header* out);

/* Writes the block's transaction count to *n. If it exceeds cap, returns
 * AL_ERR_BUFFER and writes no transactions (call again with cap >= *n). */
int32_t al_ledger_block_txs(const al_ledger* l, uint64_t height, al_tx* out, uint32_t cap,
                            uint32_t* n);

/* Unknown accounts report balance 0 and next_seq 0. */
int32_t al_ledger_account(const al_ledger* l, const uint8_t id[32], uint64_t* balance,
                          uint64_t* next_seq);

/* AL_OK if tx is valid as its sender's next transaction against the committed
 * state, otherwise AL_ERR_BALANCE / AL_ERR_SEQ / AL_ERR_OVERFLOW. */
int32_t al_ledger_check_tx(const al_ledger* l, const al_tx* tx);

/* Would txs[0..n), in this order, be valid as the transactions of the next
 * block? AL_OK, or the code of the first failing transaction (AL_ERR_TXSET
 * for a repeated txid, else AL_ERR_BALANCE / AL_ERR_SEQ / AL_ERR_OVERFLOW)
 * with its position in *bad_index. n == 0 returns AL_OK. Changes nothing.
 * Block assembly uses this: transactions that each passed al_ledger_check_tx
 * can still conflict with one another. If it returns AL_OK for n >= 1, a
 * block with these transactions, prev = hash of the tip header and the
 * matching Merkle root is accepted by al_ledger_append. */
int32_t al_ledger_check_txs(const al_ledger* l, const al_tx* txs, uint32_t n,
                            uint32_t* bad_index);

/* Validates the block as the new tip and appends it atomically, or returns one
 * of AL_ERR_LINKAGE..AL_ERR_OVERFLOW and changes nothing. Proof of work is
 * NOT checked here; the caller checks it first. */
int32_t al_ledger_append(al_ledger* l, const al_header* h, const al_tx* txs, uint32_t n);

/* Location of a confirmed transaction; AL_ERR_NOT_FOUND if unknown. */
int32_t al_ledger_find_tx(const al_ledger* l, const uint8_t txid[32], uint64_t* height,
                          uint32_t* index);

#ifdef __cplusplus
}
#endif

#endif /* ALETHEIA_LEDGER_C_H */
