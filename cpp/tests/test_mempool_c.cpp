// Tests for the fee-queue C API (cpp/capi/mempool_c.h): order and contents,
// empty-queue and argument errors, removal by txid, agreement with the C++
// class, and the reconciliation flow D2 will use after a block is appended
// (al_ledger_block_txs, then al_feeq_remove_txids, then al_ledger_check_tx).

#include "mempool_c.h"

#include "crypto_c.h"
#include "ledger_c.h"

#include "aletheia/fee_queue.h"

#include "check.h"

#include <cstdint>
#include <cstring>
#include <random>
#include <vector>

using namespace aletheia;

namespace {

void set_acct(uint8_t out[32], uint8_t n) {
    std::memset(out, 0, 32);
    out[0] = 0xAC;
    out[31] = n;
}

al_tx make_tx(uint8_t from, uint8_t to, uint64_t amount, uint64_t fee, uint64_t seq) {
    al_tx t;
    set_acct(t.sender, from);
    set_acct(t.recipient, to);
    t.amount = amount;
    t.fee = fee;
    t.seq = seq;
    return t;
}

bool same_tx(const al_tx& a, const al_tx& b) {
    return std::memcmp(a.sender, b.sender, 32) == 0 &&
           std::memcmp(a.recipient, b.recipient, 32) == 0 && a.amount == b.amount &&
           a.fee == b.fee && a.seq == b.seq;
}

Transaction to_cpp(const al_tx& c) {
    Transaction t;
    std::memcpy(t.sender.data(), c.sender, 32);
    std::memcpy(t.recipient.data(), c.recipient, 32);
    t.amount = c.amount;
    t.fee = c.fee;
    t.seq = c.seq;
    return t;
}

void test_lifecycle_and_args() {
    CHECK_EQ(al_feeq_new(nullptr), AL_ERR_ARG);
    al_feeq_free(nullptr);
    CHECK_EQ(al_feeq_size(nullptr), 0u);

    al_feeq* q = nullptr;
    CHECK_EQ(al_feeq_new(&q), AL_OK);
    CHECK(q != nullptr);
    CHECK_EQ(al_feeq_size(q), 0u);

    al_tx t = make_tx(1, 2, 10, 3, 0);
    al_tx out;
    CHECK_EQ(al_feeq_push(nullptr, &t), AL_ERR_ARG);
    CHECK_EQ(al_feeq_push(q, nullptr), AL_ERR_ARG);
    CHECK_EQ(al_feeq_pop(nullptr, &out), AL_ERR_ARG);
    CHECK_EQ(al_feeq_pop(q, nullptr), AL_ERR_ARG);
    CHECK_EQ(al_feeq_peek(nullptr, &out), AL_ERR_ARG);
    CHECK_EQ(al_feeq_peek(q, nullptr), AL_ERR_ARG);
    CHECK_EQ(al_feeq_remove_txids(nullptr, nullptr, 0), AL_ERR_ARG);
    CHECK_EQ(al_feeq_remove_txids(q, nullptr, 1), AL_ERR_ARG);
    CHECK_EQ(al_feeq_remove_txids(q, nullptr, 0), AL_OK);

    // Empty queue: NOT_FOUND, and *out is left untouched.
    al_tx sentinel = make_tx(9, 9, 9, 9, 9);
    out = sentinel;
    CHECK_EQ(al_feeq_pop(q, &out), AL_ERR_NOT_FOUND);
    CHECK(same_tx(out, sentinel));
    CHECK_EQ(al_feeq_peek(q, &out), AL_ERR_NOT_FOUND);
    CHECK(same_tx(out, sentinel));
    CHECK_EQ(al_feeq_size(q), 0u); // argument errors changed nothing

    al_feeq_free(q);
}

void test_order_and_contents() {
    al_feeq* q = nullptr;
    CHECK_EQ(al_feeq_new(&q), AL_OK);
    // Fees 4, 9, 4, UINT64_MAX, 0: the two fee-4 entries are ordered by txid.
    std::vector<al_tx> txs = {make_tx(1, 2, 100, 4, 0), make_tx(2, 3, 5, 9, 0),
                              make_tx(3, 1, 7, 4, 0), make_tx(4, 1, 1, UINT64_MAX, 0),
                              make_tx(5, 1, 0, 0, 0)};
    for (const al_tx& t : txs) CHECK_EQ(al_feeq_push(q, &t), AL_OK);
    CHECK_EQ(al_feeq_size(q), 5u);

    uint8_t id0[32], id2[32];
    al_tx_id(&txs[0], id0);
    al_tx_id(&txs[2], id2);
    const bool first_smaller = std::memcmp(id0, id2, 32) < 0;
    const al_tx want[] = {txs[3], txs[1], first_smaller ? txs[0] : txs[2],
                          first_smaller ? txs[2] : txs[0], txs[4]};

    al_tx out;
    CHECK_EQ(al_feeq_peek(q, &out), AL_OK);
    CHECK(same_tx(out, want[0]));
    CHECK_EQ(al_feeq_size(q), 5u); // peek does not remove
    for (const al_tx& w : want) {
        CHECK_EQ(al_feeq_pop(q, &out), AL_OK);
        CHECK(same_tx(out, w)); // every field survives the round trip
    }
    CHECK_EQ(al_feeq_size(q), 0u);
    CHECK_EQ(al_feeq_pop(q, &out), AL_ERR_NOT_FOUND);
    al_feeq_free(q);
}

void test_remove_txids() {
    al_feeq* q = nullptr;
    CHECK_EQ(al_feeq_new(&q), AL_OK);
    std::vector<al_tx> txs;
    for (uint8_t i = 0; i < 10; ++i) {
        txs.push_back(make_tx(i, 0, 1, i, 0));
        CHECK_EQ(al_feeq_push(q, &txs.back()), AL_OK);
    }
    CHECK_EQ(al_feeq_push(q, &txs[9]), AL_OK); // a duplicate of the top
    CHECK_EQ(al_feeq_size(q), 11u);

    // Remove txs 9 (both copies), 5, an unknown txid, and 5 again.
    uint8_t ids[4 * 32];
    al_tx_id(&txs[9], ids);
    al_tx_id(&txs[5], ids + 32);
    al_tx unknown = make_tx(200, 0, 1, 1, 0);
    al_tx_id(&unknown, ids + 64);
    al_tx_id(&txs[5], ids + 96);
    CHECK_EQ(al_feeq_remove_txids(q, ids, 4), AL_OK);
    CHECK_EQ(al_feeq_size(q), 8u);

    al_tx out;
    const uint64_t want_fees[] = {8, 7, 6, 4, 3, 2, 1, 0};
    for (uint64_t f : want_fees) {
        CHECK_EQ(al_feeq_pop(q, &out), AL_OK);
        CHECK_EQ(out.fee, f);
    }
    CHECK_EQ(al_feeq_size(q), 0u);
    CHECK_EQ(al_feeq_remove_txids(q, ids, 4), AL_OK); // on an empty queue
    al_feeq_free(q);
}

// The C API and the C++ class give the same sequence for the same operations.
void test_matches_cpp() {
    std::mt19937_64 rng(11);
    al_feeq* q = nullptr;
    CHECK_EQ(al_feeq_new(&q), AL_OK);
    FeePriorityQueue ref;
    bool ok = true;
    for (int step = 0; step < 5000; ++step) {
        const unsigned op = static_cast<unsigned>(rng() % 4);
        if (op < 2) {
            al_tx t = make_tx(static_cast<uint8_t>(rng() % 16), 1, rng() % 100, rng() % 5,
                              static_cast<uint64_t>(step));
            ok = ok && al_feeq_push(q, &t) == AL_OK;
            ref.push(to_cpp(t));
        } else if (op == 2) {
            al_tx out;
            const int32_t s = al_feeq_pop(q, &out);
            if (ref.empty()) {
                ok = ok && s == AL_ERR_NOT_FOUND;
            } else {
                ok = ok && s == AL_OK && to_cpp(out) == ref.pop().tx;
            }
        } else if (!ref.empty()) {
            const Hash256 id = ref.top().txid;
            ok = ok && al_feeq_remove_txids(q, id.data(), 1) == AL_OK;
            ref.remove_txids({id});
        }
        ok = ok && al_feeq_size(q) == ref.size();
        if (!ok) break;
    }
    CHECK(ok);
    al_feeq_free(q);
}

// Reconciliation as specified for D2 (DOMAINS_AND_COMMITS §3.1): after a block
// is appended, drop its txids from the queue; what remains is re-checked.
void test_reconciliation_flow() {
    al_alloc allocs[2];
    set_acct(allocs[0].account, 1);
    allocs[0].balance = 1000;
    set_acct(allocs[1].account, 2);
    allocs[1].balance = 1000;
    al_header g;
    std::memset(&g, 0, sizeof g);
    g.version = 1;
    al_ledger* l = nullptr;
    CHECK_EQ(al_ledger_new(&g, allocs, 2, &l), AL_OK);

    al_feeq* q = nullptr;
    CHECK_EQ(al_feeq_new(&q), AL_OK);
    const al_tx a = make_tx(1, 2, 10, 5, 0); // will be mined
    const al_tx b = make_tx(2, 1, 10, 7, 0); // will be mined
    const al_tx c = make_tx(1, 2, 10, 9, 0); // conflicts with a (same seq)
    const al_tx d = make_tx(1, 2, 10, 1, 1); // a's successor
    for (const al_tx* t : {&a, &b, &c, &d}) CHECK_EQ(al_feeq_push(q, t), AL_OK);

    // Mine a block holding a and b.
    al_tx block[2] = {a, b};
    al_header h = g;
    al_header_hash(&g, h.prev);
    uint8_t leaves[64];
    al_tx_id(&a, leaves);
    al_tx_id(&b, leaves + 32);
    CHECK_EQ(al_merkle_root(leaves, 2, h.merkle_root), AL_OK);
    h.timestamp = 1;
    CHECK_EQ(al_ledger_append(l, &h, block, 2), AL_OK);

    // 1. read the block's transactions; 2. remove their txids from the queue.
    uint32_t n = 0;
    CHECK_EQ(al_ledger_block_txs(l, 1, nullptr, 0, &n), AL_ERR_BUFFER);
    std::vector<al_tx> mined(n);
    CHECK_EQ(al_ledger_block_txs(l, 1, mined.data(), n, &n), AL_OK);
    std::vector<uint8_t> ids(32 * static_cast<size_t>(n));
    for (uint32_t i = 0; i < n; ++i) al_tx_id(&mined[i], &ids[32 * i]);
    CHECK_EQ(al_feeq_remove_txids(q, ids.data(), n), AL_OK);
    CHECK_EQ(al_feeq_size(q), 2u);

    // 3. re-check the rest against the new state: c is now stale, d is valid.
    al_tx out;
    CHECK_EQ(al_feeq_pop(q, &out), AL_OK);
    CHECK(same_tx(out, c));
    CHECK_EQ(al_ledger_check_tx(l, &out), AL_ERR_SEQ);
    CHECK_EQ(al_feeq_pop(q, &out), AL_OK);
    CHECK(same_tx(out, d));
    CHECK_EQ(al_ledger_check_tx(l, &out), AL_OK);

    al_feeq_free(q);
    al_ledger_free(l);
}

} // namespace

int main() {
    test_lifecycle_and_args();
    test_order_and_contents();
    test_remove_txids();
    test_matches_cpp();
    test_reconciliation_flow();
    return aletheia_test::finish("test_mempool_c");
}
