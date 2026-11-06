#include "aletheia/block.h"

#include "aletheia/sha256.h"

#include <cstring>

namespace aletheia {

namespace {

// Big-endian writers/readers for fixed-width integers. `width` is in bytes.
void put_be(std::uint8_t* out, std::uint64_t v, int width) {
    for (int i = width - 1; i >= 0; --i) {
        out[i] = static_cast<std::uint8_t>(v & 0xff);
        v >>= 8;
    }
}

std::uint64_t get_be(const std::uint8_t* in, int width) {
    std::uint64_t v = 0;
    for (int i = 0; i < width; ++i) v = (v << 8) | in[i];
    return v;
}

void put_hash(std::uint8_t* out, const Hash256& h) { std::memcpy(out, h.data(), 32); }

Hash256 get_hash(const std::uint8_t* in) {
    Hash256 h;
    std::memcpy(h.data(), in, 32);
    return h;
}

} // namespace

bool operator==(const Transaction& a, const Transaction& b) {
    return a.sender == b.sender && a.recipient == b.recipient && a.amount == b.amount &&
           a.fee == b.fee && a.seq == b.seq;
}
bool operator!=(const Transaction& a, const Transaction& b) { return !(a == b); }

bool operator==(const BlockHeader& a, const BlockHeader& b) {
    return a.version == b.version && a.prev == b.prev && a.merkle_root == b.merkle_root &&
           a.timestamp == b.timestamp && a.difficulty == b.difficulty && a.nonce == b.nonce;
}
bool operator!=(const BlockHeader& a, const BlockHeader& b) { return !(a == b); }

TxBytes encode(const Transaction& tx) {
    TxBytes out;
    put_hash(&out[0], tx.sender);
    put_hash(&out[32], tx.recipient);
    put_be(&out[64], tx.amount, 8);
    put_be(&out[72], tx.fee, 8);
    put_be(&out[80], tx.seq, 8);
    return out;
}

Transaction decode_tx(const std::uint8_t* in) {
    Transaction tx;
    tx.sender = get_hash(in);
    tx.recipient = get_hash(in + 32);
    tx.amount = get_be(in + 64, 8);
    tx.fee = get_be(in + 72, 8);
    tx.seq = get_be(in + 80, 8);
    return tx;
}

HeaderBytes encode(const BlockHeader& h) {
    HeaderBytes out;
    put_be(&out[0], h.version, 4);
    put_hash(&out[4], h.prev);
    put_hash(&out[36], h.merkle_root);
    // Two's-complement bit pattern of the signed timestamp.
    put_be(&out[68], static_cast<std::uint64_t>(h.timestamp), 8);
    put_be(&out[76], h.difficulty, 4);
    put_be(&out[80], h.nonce, 8);
    return out;
}

BlockHeader decode_header(const std::uint8_t* in) {
    BlockHeader h;
    h.version = static_cast<std::uint32_t>(get_be(in, 4));
    h.prev = get_hash(in + 4);
    h.merkle_root = get_hash(in + 36);
    h.timestamp = static_cast<std::int64_t>(get_be(in + 68, 8));
    h.difficulty = static_cast<std::uint32_t>(get_be(in + 76, 4));
    h.nonce = get_be(in + 80, 8);
    return h;
}

Hash256 tx_id(const Transaction& tx) {
    const TxBytes b = encode(tx);
    return SHA256::hash(b.data(), b.size());
}

Hash256 header_hash(const BlockHeader& h) {
    const HeaderBytes b = encode(h);
    return SHA256::hash(b.data(), b.size());
}

std::vector<Hash256> tx_ids(const std::vector<Transaction>& txs) {
    std::vector<Hash256> ids;
    ids.reserve(txs.size());
    for (const Transaction& tx : txs) ids.push_back(tx_id(tx));
    return ids;
}

} // namespace aletheia
