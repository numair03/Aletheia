// Tests for transaction/header types and their canonical encodings
// (cpp/include/aletheia/block.h). Expected bytes and hashes were computed
// independently with Python's struct.pack('>...') and hashlib.sha256.

#include "aletheia/block.h"
#include "aletheia/merkle.h"
#include "aletheia/sha256.h"

#include "check.h"

#include <cstdint>
#include <random>
#include <string>

using namespace aletheia;

namespace {

std::string hex_of(const std::uint8_t* p, std::size_t n) {
    static const char* d = "0123456789abcdef";
    std::string s;
    for (std::size_t i = 0; i < n; ++i) {
        s += d[p[i] >> 4];
        s += d[p[i] & 15];
    }
    return s;
}

Transaction sample_tx() {
    Transaction tx;
    for (int i = 0; i < 32; ++i) {
        tx.sender[i] = static_cast<std::uint8_t>(i);
        tx.recipient[i] = static_cast<std::uint8_t>(32 + i);
    }
    tx.amount = 1000;
    tx.fee = 0x0102030405060708ULL;
    tx.seq = 0xFFFFFFFFFFFFFFFFULL;
    return tx;
}

BlockHeader sample_header() {
    BlockHeader h;
    h.version = 1;
    h.prev.fill(0xAA);
    h.merkle_root.fill(0xBB);
    h.timestamp = -2; // exercises the signed field's bit pattern
    h.difficulty = 20;
    h.nonce = 0xDEADBEEFULL;
    return h;
}

void test_tx_encoding() {
    const Transaction tx = sample_tx();
    const TxBytes b = encode(tx);
    CHECK_EQ(hex_of(b.data(), 32), to_hex(tx.sender));
    CHECK_EQ(hex_of(b.data() + 32, 32), to_hex(tx.recipient));
    CHECK_EQ(hex_of(b.data() + 64, 24),
             std::string("00000000000003e80102030405060708ffffffffffffffff"));
    CHECK_EQ(to_hex(tx_id(tx)),
             std::string("5c242f353249fbaed47cf55c805566e1f96567fb26b6de936bbf00c4c960331e"));
    CHECK(decode_tx(b.data()) == tx);
}

void test_header_encoding() {
    const BlockHeader h = sample_header();
    const HeaderBytes b = encode(h);
    CHECK_EQ(hex_of(b.data(), 4), std::string("00000001"));
    CHECK_EQ(hex_of(b.data() + 4, 32), std::string(64, 'a'));
    CHECK_EQ(hex_of(b.data() + 36, 32), std::string(64, 'b'));
    CHECK_EQ(hex_of(b.data() + 68, 20), std::string("fffffffffffffffe0000001400000000deadbeef"));
    CHECK_EQ(to_hex(header_hash(h)),
             std::string("157d2701231da08ba62f42d5b157c6dc65aa9d6d93926bd3e6c2f390ee113b15"));
    CHECK(decode_header(b.data()) == h);

    // The default (all-zero) header hashes to SHA-256 of 88 zero bytes.
    CHECK_EQ(to_hex(header_hash(BlockHeader{})),
             std::string("10eef285deef7a4b7c82b22aa53589b7833df29de3814649c772bbd5c832f365"));
}

// Every field is covered by the hash: changing any one changes the txid or
// block hash. (Guards against a field silently left out of the encoding.)
void test_every_field_hashed() {
    const Transaction tx = sample_tx();
    const Hash256 id = tx_id(tx);
    Transaction t = tx; t.sender[5] ^= 1;    CHECK(tx_id(t) != id);
    t = tx; t.recipient[31] ^= 1;            CHECK(tx_id(t) != id);
    t = tx; t.amount ^= 1;                   CHECK(tx_id(t) != id);
    t = tx; t.fee ^= 1;                      CHECK(tx_id(t) != id);
    t = tx; t.seq ^= 1;                      CHECK(tx_id(t) != id);

    const BlockHeader h = sample_header();
    const Hash256 hh = header_hash(h);
    BlockHeader g = h; g.version ^= 1;       CHECK(header_hash(g) != hh);
    g = h; g.prev[0] ^= 1;                   CHECK(header_hash(g) != hh);
    g = h; g.merkle_root[31] ^= 1;           CHECK(header_hash(g) != hh);
    g = h; g.timestamp ^= 1;                 CHECK(header_hash(g) != hh);
    g = h; g.difficulty ^= 1;                CHECK(header_hash(g) != hh);
    g = h; g.nonce ^= 1;                     CHECK(header_hash(g) != hh);
}

// encode(decode(bytes)) == bytes for arbitrary 88-byte inputs: every byte
// pattern is a valid encoding, so decoding never loses information.
void test_round_trip_random() {
    std::mt19937 rng(7);
    bool ok = true;
    for (int i = 0; i < 1000; ++i) {
        std::uint8_t raw[88];
        for (std::uint8_t& c : raw) c = static_cast<std::uint8_t>(rng());
        const TxBytes tb = encode(decode_tx(raw));
        const HeaderBytes hb = encode(decode_header(raw));
        for (int j = 0; j < 88; ++j) ok = ok && tb[j] == raw[j] && hb[j] == raw[j];
    }
    CHECK(ok);
}

void test_tx_ids_and_equality() {
    Transaction a = sample_tx();
    Transaction b = a;
    b.seq = 0;
    const std::vector<Hash256> ids = tx_ids({a, b});
    CHECK_EQ(ids.size(), 2u);
    CHECK(ids[0] == tx_id(a));
    CHECK(ids[1] == tx_id(b));
    CHECK(tx_ids({}).empty());
    CHECK(a == a);
    CHECK(a != b);
    BlockHeader h = sample_header();
    BlockHeader g = h;
    CHECK(h == g);
    g.nonce++;
    CHECK(h != g);

    // A block's Merkle root is built over its txids (M-R1).
    CHECK(MerkleTree(ids).root() == SHA256::hash(ids[0], ids[1]));
}

} // namespace

int main() {
    test_tx_encoding();
    test_header_encoding();
    test_every_field_hashed();
    test_round_trip_random();
    test_tx_ids_and_equality();
    return aletheia_test::finish("test_block");
}
