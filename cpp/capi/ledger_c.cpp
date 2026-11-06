#include "ledger_c.h"

#include "aletheia/block.h"
#include "aletheia/ledger.h"

#include <cstring>
#include <stdexcept>
#include <vector>

using namespace aletheia;
using aletheia::capi::guarded;

// The opaque handle is the C++ ledger itself.
struct al_ledger {
    Ledger ledger;
};

namespace {

Hash256 load_hash(const uint8_t* p) {
    Hash256 h;
    std::memcpy(h.data(), p, h.size());
    return h;
}

Transaction from_c(const al_tx& c) {
    Transaction t;
    t.sender = load_hash(c.sender);
    t.recipient = load_hash(c.recipient);
    t.amount = c.amount;
    t.fee = c.fee;
    t.seq = c.seq;
    return t;
}

al_tx to_c(const Transaction& t) {
    al_tx c;
    std::memcpy(c.sender, t.sender.data(), 32);
    std::memcpy(c.recipient, t.recipient.data(), 32);
    c.amount = t.amount;
    c.fee = t.fee;
    c.seq = t.seq;
    return c;
}

BlockHeader from_c(const al_header& c) {
    BlockHeader h;
    h.version = c.version;
    h.prev = load_hash(c.prev);
    h.merkle_root = load_hash(c.merkle_root);
    h.timestamp = c.timestamp;
    h.difficulty = c.difficulty;
    h.nonce = c.nonce;
    return h;
}

al_header to_c(const BlockHeader& h) {
    al_header c;
    c.version = h.version;
    std::memcpy(c.prev, h.prev.data(), 32);
    std::memcpy(c.merkle_root, h.merkle_root.data(), 32);
    c.timestamp = h.timestamp;
    c.difficulty = h.difficulty;
    c.nonce = h.nonce;
    return c;
}

int32_t status_of(RuleError e) {
    switch (e) {
    case RuleError::None: return AL_OK;
    case RuleError::Linkage: return AL_ERR_LINKAGE;
    case RuleError::Merkle: return AL_ERR_MERKLE;
    case RuleError::TxSet: return AL_ERR_TXSET;
    case RuleError::Balance: return AL_ERR_BALANCE;
    case RuleError::Seq: return AL_ERR_SEQ;
    case RuleError::Overflow: return AL_ERR_OVERFLOW;
    }
    return AL_ERR_INTERNAL;
}

} // namespace

extern "C" {

int32_t al_tx_encode(const al_tx* tx, uint8_t out[AL_TX_SIZE]) {
    return guarded([&]() -> int32_t {
        if (tx == nullptr || out == nullptr) return AL_ERR_ARG;
        const TxBytes b = encode(from_c(*tx));
        std::memcpy(out, b.data(), b.size());
        return AL_OK;
    });
}

int32_t al_tx_decode(const uint8_t in[AL_TX_SIZE], al_tx* out) {
    return guarded([&]() -> int32_t {
        if (in == nullptr || out == nullptr) return AL_ERR_ARG;
        *out = to_c(decode_tx(in));
        return AL_OK;
    });
}

int32_t al_tx_id(const al_tx* tx, uint8_t out[32]) {
    return guarded([&]() -> int32_t {
        if (tx == nullptr || out == nullptr) return AL_ERR_ARG;
        const Hash256 id = tx_id(from_c(*tx));
        std::memcpy(out, id.data(), id.size());
        return AL_OK;
    });
}

int32_t al_header_encode(const al_header* h, uint8_t out[AL_HEADER_SIZE]) {
    return guarded([&]() -> int32_t {
        if (h == nullptr || out == nullptr) return AL_ERR_ARG;
        const HeaderBytes b = encode(from_c(*h));
        std::memcpy(out, b.data(), b.size());
        return AL_OK;
    });
}

int32_t al_header_decode(const uint8_t in[AL_HEADER_SIZE], al_header* out) {
    return guarded([&]() -> int32_t {
        if (in == nullptr || out == nullptr) return AL_ERR_ARG;
        *out = to_c(decode_header(in));
        return AL_OK;
    });
}

int32_t al_header_hash(const al_header* h, uint8_t out[32]) {
    return guarded([&]() -> int32_t {
        if (h == nullptr || out == nullptr) return AL_ERR_ARG;
        const Hash256 hh = header_hash(from_c(*h));
        std::memcpy(out, hh.data(), hh.size());
        return AL_OK;
    });
}

int32_t al_ledger_new(const al_header* genesis, const al_alloc* allocs, uint32_t n,
                      al_ledger** out) {
    if (out != nullptr) *out = nullptr;
    return guarded([&]() -> int32_t {
        if (genesis == nullptr || out == nullptr || (allocs == nullptr && n != 0)) return AL_ERR_ARG;
        std::vector<Allocation> v(n);
        for (uint32_t i = 0; i < n; ++i) v[i] = Allocation{load_hash(allocs[i].account), allocs[i].balance};
        try {
            *out = new al_ledger{Ledger(from_c(*genesis), v)};
        } catch (const std::invalid_argument&) {
            return AL_ERR_ARG;
        }
        return AL_OK;
    });
}

void al_ledger_free(al_ledger* l) { delete l; }

uint64_t al_ledger_height(const al_ledger* l) { return l == nullptr ? 0 : l->ledger.height(); }

int32_t al_ledger_header_at(const al_ledger* l, uint64_t height, al_header* out) {
    return guarded([&]() -> int32_t {
        if (l == nullptr || out == nullptr) return AL_ERR_ARG;
        if (height > l->ledger.height()) return AL_ERR_NOT_FOUND;
        *out = to_c(l->ledger.header_at(height));
        return AL_OK;
    });
}

int32_t al_ledger_block_txs(const al_ledger* l, uint64_t height, al_tx* out, uint32_t cap,
                            uint32_t* n) {
    return guarded([&]() -> int32_t {
        if (l == nullptr || n == nullptr || (out == nullptr && cap != 0)) return AL_ERR_ARG;
        if (height > l->ledger.height()) return AL_ERR_NOT_FOUND;
        const std::vector<Transaction>& txs = l->ledger.txs_at(height);
        *n = static_cast<uint32_t>(txs.size()); // append() caps blocks at UINT32_MAX txs
        if (txs.size() > cap) return AL_ERR_BUFFER;
        for (size_t i = 0; i < txs.size(); ++i) out[i] = to_c(txs[i]);
        return AL_OK;
    });
}

int32_t al_ledger_account(const al_ledger* l, const uint8_t id[32], uint64_t* balance,
                          uint64_t* next_seq) {
    return guarded([&]() -> int32_t {
        if (l == nullptr || id == nullptr || balance == nullptr || next_seq == nullptr) return AL_ERR_ARG;
        const Account a = l->ledger.account(load_hash(id));
        *balance = a.balance;
        *next_seq = a.next_seq;
        return AL_OK;
    });
}

int32_t al_ledger_check_tx(const al_ledger* l, const al_tx* tx) {
    return guarded([&]() -> int32_t {
        if (l == nullptr || tx == nullptr) return AL_ERR_ARG;
        return status_of(l->ledger.check_tx(from_c(*tx)));
    });
}

int32_t al_ledger_check_txs(const al_ledger* l, const al_tx* txs, uint32_t n,
                            uint32_t* bad_index) {
    return guarded([&]() -> int32_t {
        if (l == nullptr || bad_index == nullptr || (txs == nullptr && n != 0)) return AL_ERR_ARG;
        std::vector<Transaction> v;
        v.reserve(n);
        for (uint32_t i = 0; i < n; ++i) v.push_back(from_c(txs[i]));
        std::size_t bad = 0;
        const RuleError e = l->ledger.check_txs(v, &bad);
        if (e != RuleError::None) *bad_index = static_cast<uint32_t>(bad); // bad < n
        return status_of(e);
    });
}

int32_t al_ledger_append(al_ledger* l, const al_header* h, const al_tx* txs, uint32_t n) {
    return guarded([&]() -> int32_t {
        if (l == nullptr || h == nullptr || (txs == nullptr && n != 0)) return AL_ERR_ARG;
        Block b;
        b.header = from_c(*h);
        b.txs.reserve(n);
        for (uint32_t i = 0; i < n; ++i) b.txs.push_back(from_c(txs[i]));
        return status_of(l->ledger.append(b));
    });
}

int32_t al_ledger_find_tx(const al_ledger* l, const uint8_t txid[32], uint64_t* height,
                          uint32_t* index) {
    return guarded([&]() -> int32_t {
        if (l == nullptr || txid == nullptr || height == nullptr || index == nullptr) return AL_ERR_ARG;
        const auto loc = l->ledger.find_tx(load_hash(txid));
        if (!loc) return AL_ERR_NOT_FOUND;
        *height = loc->height;
        *index = loc->index;
        return AL_OK;
    });
}

} // extern "C"
