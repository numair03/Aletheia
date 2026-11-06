// Tests for the ledger C API (cpp/capi/ledger_c.h): agreement with the C++
// classes, the buffer protocol, rule-to-status mapping, and argument errors.

#include "ledger_c.h"

#include "aletheia/block.h"
#include "aletheia/merkle.h"

#include "check.h"

#include <cstdint>
#include <cstring>
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

al_header genesis() {
    al_header g;
    std::memset(&g, 0, sizeof g);
    g.version = 1;
    g.timestamp = 1700000000;
    g.difficulty = 16;
    return g;
}

al_ledger* new_ledger() {
    al_alloc allocs[2];
    set_acct(allocs[0].account, 1);
    allocs[0].balance = 1000;
    set_acct(allocs[1].account, 2);
    allocs[1].balance = 500;
    al_header g = genesis();
    al_ledger* l = nullptr;
    CHECK_EQ(al_ledger_new(&g, allocs, 2, &l), AL_OK);
    return l;
}

// A linked header with the correct Merkle root, built only through the C API.
al_header next_header(const al_ledger* l, const std::vector<al_tx>& txs) {
    al_header tip;
    al_ledger_header_at(l, al_ledger_height(l), &tip);
    al_header h = genesis();
    al_header_hash(&tip, h.prev);
    std::vector<uint8_t> leaves(32 * txs.size());
    for (size_t i = 0; i < txs.size(); ++i) al_tx_id(&txs[i], &leaves[32 * i]);
    std::vector<Hash256> ids(txs.size());
    for (size_t i = 0; i < txs.size(); ++i) std::memcpy(ids[i].data(), &leaves[32 * i], 32);
    const Hash256 root = MerkleTree(ids).root();
    std::memcpy(h.merkle_root, root.data(), 32);
    h.timestamp = tip.timestamp + 1;
    return h;
}

int32_t append(al_ledger* l, const std::vector<al_tx>& txs) {
    al_header h = next_header(l, txs);
    return al_ledger_append(l, &h, txs.data(), static_cast<uint32_t>(txs.size()));
}

void test_encodings_match_cpp() {
    al_tx t = make_tx(1, 2, 1000, 0x0102030405060708ULL, 77);
    uint8_t enc[AL_TX_SIZE];
    CHECK_EQ(al_tx_encode(&t, enc), AL_OK);

    Transaction cpp;
    std::memcpy(cpp.sender.data(), t.sender, 32);
    std::memcpy(cpp.recipient.data(), t.recipient, 32);
    cpp.amount = t.amount;
    cpp.fee = t.fee;
    cpp.seq = t.seq;
    const TxBytes expected = encode(cpp);
    CHECK(std::memcmp(enc, expected.data(), AL_TX_SIZE) == 0);

    uint8_t id[32];
    CHECK_EQ(al_tx_id(&t, id), AL_OK);
    CHECK(std::memcmp(id, tx_id(cpp).data(), 32) == 0);

    al_tx back;
    CHECK_EQ(al_tx_decode(enc, &back), AL_OK);
    CHECK(std::memcmp(&back.sender, t.sender, 32) == 0 && back.amount == t.amount &&
          back.fee == t.fee && back.seq == t.seq && std::memcmp(back.recipient, t.recipient, 32) == 0);

    al_header h = genesis();
    h.prev[3] = 9;
    h.timestamp = -5;
    h.nonce = 123456789;
    uint8_t henc[AL_HEADER_SIZE];
    CHECK_EQ(al_header_encode(&h, henc), AL_OK);
    al_header hb;
    CHECK_EQ(al_header_decode(henc, &hb), AL_OK);
    CHECK(hb.version == h.version && std::memcmp(hb.prev, h.prev, 32) == 0 &&
          std::memcmp(hb.merkle_root, h.merkle_root, 32) == 0 && hb.timestamp == h.timestamp &&
          hb.difficulty == h.difficulty && hb.nonce == h.nonce);
    uint8_t hh[32];
    CHECK_EQ(al_header_hash(&h, hh), AL_OK);
    CHECK(std::memcmp(hh, header_hash(decode_header(henc)).data(), 32) == 0);
}

void test_ledger_flow() {
    al_ledger* l = new_ledger();
    CHECK(l != nullptr);
    CHECK_EQ(al_ledger_height(l), 0u);

    uint8_t a[32], b[32], c[32];
    set_acct(a, 1);
    set_acct(b, 2);
    set_acct(c, 3);
    uint64_t bal = 0, seq = 0;
    CHECK_EQ(al_ledger_account(l, a, &bal, &seq), AL_OK);
    CHECK(bal == 1000 && seq == 0);
    CHECK_EQ(al_ledger_account(l, c, &bal, &seq), AL_OK);
    CHECK(bal == 0 && seq == 0);

    const std::vector<al_tx> txs = {make_tx(1, 3, 100, 2, 0), make_tx(2, 3, 50, 1, 0),
                                    make_tx(1, 2, 10, 0, 1)};
    CHECK_EQ(al_ledger_check_tx(l, &txs[0]), AL_OK);
    CHECK_EQ(append(l, txs), AL_OK);
    CHECK_EQ(al_ledger_height(l), 1u);
    CHECK_EQ(al_ledger_account(l, a, &bal, &seq), AL_OK);
    CHECK(bal == 888 && seq == 2);
    CHECK_EQ(al_ledger_account(l, c, &bal, &seq), AL_OK);
    CHECK(bal == 150 && seq == 0);

    // Buffer protocol: size query, too-small buffer, exact buffer.
    uint32_t n = 0;
    CHECK_EQ(al_ledger_block_txs(l, 1, nullptr, 0, &n), AL_ERR_BUFFER);
    CHECK_EQ(n, 3u);
    al_tx out[3];
    CHECK_EQ(al_ledger_block_txs(l, 1, out, 2, &n), AL_ERR_BUFFER);
    CHECK_EQ(al_ledger_block_txs(l, 1, out, 3, &n), AL_OK);
    CHECK(out[2].amount == 10 && out[2].seq == 1 && std::memcmp(out[2].recipient, b, 32) == 0);
    CHECK_EQ(al_ledger_block_txs(l, 0, nullptr, 0, &n), AL_OK); // genesis: zero txs
    CHECK_EQ(n, 0u);
    CHECK_EQ(al_ledger_block_txs(l, 2, out, 3, &n), AL_ERR_NOT_FOUND);

    // Header lookup and linkage.
    al_header g, h1;
    CHECK_EQ(al_ledger_header_at(l, 0, &g), AL_OK);
    CHECK_EQ(al_ledger_header_at(l, 1, &h1), AL_OK);
    uint8_t gh[32];
    al_header_hash(&g, gh);
    CHECK(std::memcmp(h1.prev, gh, 32) == 0);
    CHECK_EQ(al_ledger_header_at(l, 2, &h1), AL_ERR_NOT_FOUND);

    // Transaction index.
    uint8_t id[32];
    al_tx_id(&txs[2], id);
    uint64_t height = 0;
    uint32_t index = 0;
    CHECK_EQ(al_ledger_find_tx(l, id, &height, &index), AL_OK);
    CHECK(height == 1 && index == 2);
    id[0] ^= 1;
    CHECK_EQ(al_ledger_find_tx(l, id, &height, &index), AL_ERR_NOT_FOUND);

    al_ledger_free(l);
}

void test_rule_statuses() {
    al_ledger* l = new_ledger();
    const al_tx ok = make_tx(1, 2, 1, 1, 0);

    al_header h = next_header(l, {ok});
    h.prev[0] ^= 1;
    CHECK_EQ(al_ledger_append(l, &h, &ok, 1), AL_ERR_LINKAGE);
    h = next_header(l, {ok});
    h.merkle_root[0] ^= 1;
    CHECK_EQ(al_ledger_append(l, &h, &ok, 1), AL_ERR_MERKLE);
    CHECK_EQ(append(l, {}), AL_ERR_TXSET);
    CHECK_EQ(append(l, {ok, ok}), AL_ERR_TXSET);
    CHECK_EQ(append(l, {make_tx(1, 2, 1000, 1, 0)}), AL_ERR_BALANCE);
    CHECK_EQ(append(l, {make_tx(1, 2, 1, 1, 5)}), AL_ERR_SEQ);
    CHECK_EQ(append(l, {make_tx(1, 2, UINT64_MAX, 1, 0)}), AL_ERR_OVERFLOW);
    CHECK_EQ(al_ledger_height(l), 0u);

    const al_tx broke = make_tx(3, 1, 1, 0, 0);
    const al_tx late = make_tx(1, 2, 1, 1, 1);
    CHECK_EQ(al_ledger_check_tx(l, &broke), AL_ERR_BALANCE);
    CHECK_EQ(al_ledger_check_tx(l, &late), AL_ERR_SEQ);

    // List check: individually admissible transactions that conflict.
    const al_tx dup_seq[2] = {make_tx(1, 2, 10, 0, 0), make_tx(1, 3, 10, 0, 0)};
    uint32_t bad = 77;
    CHECK_EQ(al_ledger_check_txs(l, dup_seq, 2, &bad), AL_ERR_SEQ);
    CHECK_EQ(bad, 1u);
    const al_tx twice[2] = {ok, ok};
    CHECK_EQ(al_ledger_check_txs(l, twice, 2, &bad), AL_ERR_TXSET);
    CHECK_EQ(bad, 1u);
    const al_tx fine[2] = {ok, make_tx(2, 1, 5, 0, 0)};
    bad = 77;
    CHECK_EQ(al_ledger_check_txs(l, fine, 2, &bad), AL_OK);
    CHECK_EQ(bad, 77u);
    CHECK_EQ(al_ledger_check_txs(l, nullptr, 0, &bad), AL_OK);
    CHECK_EQ(append(l, {fine[0], fine[1]}), AL_OK); // accepted as predicted
    al_ledger_free(l);
}

void test_argument_errors() {
    al_tx t = make_tx(1, 2, 1, 1, 0);
    al_header g = genesis();
    uint8_t buf[88];
    CHECK_EQ(al_tx_encode(nullptr, buf), AL_ERR_ARG);
    CHECK_EQ(al_tx_encode(&t, nullptr), AL_ERR_ARG);
    CHECK_EQ(al_tx_decode(nullptr, &t), AL_ERR_ARG);
    CHECK_EQ(al_tx_id(&t, nullptr), AL_ERR_ARG);
    CHECK_EQ(al_header_encode(nullptr, buf), AL_ERR_ARG);
    CHECK_EQ(al_header_decode(buf, nullptr), AL_ERR_ARG);
    CHECK_EQ(al_header_hash(nullptr, buf), AL_ERR_ARG);

    // al_ledger_new: bad arguments and bad genesis leave *out NULL.
    al_ledger* l = reinterpret_cast<al_ledger*>(&g); // non-null sentinel
    CHECK_EQ(al_ledger_new(nullptr, nullptr, 0, &l), AL_ERR_ARG);
    CHECK(l == nullptr);
    CHECK_EQ(al_ledger_new(&g, nullptr, 1, &l), AL_ERR_ARG);
    CHECK_EQ(al_ledger_new(&g, nullptr, 0, nullptr), AL_ERR_ARG);
    al_header bad = g;
    bad.merkle_root[0] = 1;
    CHECK_EQ(al_ledger_new(&bad, nullptr, 0, &l), AL_ERR_ARG);
    CHECK(l == nullptr);
    al_alloc dup[2];
    set_acct(dup[0].account, 1);
    dup[0].balance = 1;
    dup[1] = dup[0];
    CHECK_EQ(al_ledger_new(&g, dup, 2, &l), AL_ERR_ARG);
    CHECK(l == nullptr);
    CHECK_EQ(al_ledger_new(&g, nullptr, 0, &l), AL_OK); // no allocations is fine
    CHECK(l != nullptr);

    uint64_t u = 0;
    uint32_t n = 0;
    al_header h;
    CHECK_EQ(al_ledger_height(nullptr), 0u);
    CHECK_EQ(al_ledger_header_at(nullptr, 0, &h), AL_ERR_ARG);
    CHECK_EQ(al_ledger_header_at(l, 0, nullptr), AL_ERR_ARG);
    CHECK_EQ(al_ledger_block_txs(l, 0, nullptr, 1, &n), AL_ERR_ARG);
    CHECK_EQ(al_ledger_block_txs(l, 0, nullptr, 0, nullptr), AL_ERR_ARG);
    CHECK_EQ(al_ledger_account(l, nullptr, &u, &u), AL_ERR_ARG);
    CHECK_EQ(al_ledger_account(l, buf, nullptr, &u), AL_ERR_ARG);
    CHECK_EQ(al_ledger_check_tx(l, nullptr), AL_ERR_ARG);
    CHECK_EQ(al_ledger_check_txs(l, &t, 1, nullptr), AL_ERR_ARG);
    CHECK_EQ(al_ledger_check_txs(l, nullptr, 1, &n), AL_ERR_ARG);
    CHECK_EQ(al_ledger_check_txs(nullptr, &t, 1, &n), AL_ERR_ARG);
    CHECK_EQ(al_ledger_append(l, nullptr, &t, 1), AL_ERR_ARG);
    CHECK_EQ(al_ledger_append(l, &g, nullptr, 1), AL_ERR_ARG);
    CHECK_EQ(al_ledger_find_tx(l, buf, nullptr, &n), AL_ERR_ARG);
    CHECK_EQ(al_ledger_find_tx(nullptr, buf, &u, &n), AL_ERR_ARG);
    al_ledger_free(l);
    al_ledger_free(nullptr); // ignored
}

} // namespace

int main() {
    test_encodings_match_cpp();
    test_ledger_flow();
    test_rule_statuses();
    test_argument_errors();
    return aletheia_test::finish("test_ledger_c");
}
