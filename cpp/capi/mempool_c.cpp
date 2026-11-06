#include "mempool_c.h"

#include "aletheia/block.h"
#include "aletheia/fee_queue.h"

#include <cstring>
#include <vector>

using namespace aletheia;
using aletheia::capi::guarded;

// The opaque handle is the C++ queue itself.
struct al_feeq {
    FeePriorityQueue queue;
};

namespace {

// Same field-by-field conversion as ledger_c.cpp; the struct layout is never
// hashed, only the canonical encoding computed by block.cpp.
Transaction from_c(const al_tx& c) {
    Transaction t;
    std::memcpy(t.sender.data(), c.sender, 32);
    std::memcpy(t.recipient.data(), c.recipient, 32);
    t.amount = c.amount;
    t.fee = c.fee;
    t.seq = c.seq;
    return t;
}

void to_c(const Transaction& t, al_tx* out) {
    std::memcpy(out->sender, t.sender.data(), 32);
    std::memcpy(out->recipient, t.recipient.data(), 32);
    out->amount = t.amount;
    out->fee = t.fee;
    out->seq = t.seq;
}

} // namespace

extern "C" {

int32_t al_feeq_new(al_feeq** out) {
    if (out == nullptr) {
        return AL_ERR_ARG;
    }
    *out = nullptr;
    return guarded([&]() -> int32_t {
        *out = new al_feeq{};
        return AL_OK;
    });
}

void al_feeq_free(al_feeq* q) {
    delete q;
}

int32_t al_feeq_push(al_feeq* q, const al_tx* tx) {
    if (q == nullptr || tx == nullptr) {
        return AL_ERR_ARG;
    }
    return guarded([&]() -> int32_t {
        q->queue.push(from_c(*tx));
        return AL_OK;
    });
}

int32_t al_feeq_pop(al_feeq* q, al_tx* out) {
    if (q == nullptr || out == nullptr) {
        return AL_ERR_ARG;
    }
    return guarded([&]() -> int32_t {
        if (q->queue.empty()) {
            return AL_ERR_NOT_FOUND;
        }
        to_c(q->queue.pop().tx, out);
        return AL_OK;
    });
}

int32_t al_feeq_peek(const al_feeq* q, al_tx* out) {
    if (q == nullptr || out == nullptr) {
        return AL_ERR_ARG;
    }
    return guarded([&]() -> int32_t {
        if (q->queue.empty()) {
            return AL_ERR_NOT_FOUND;
        }
        to_c(q->queue.top().tx, out);
        return AL_OK;
    });
}

uint64_t al_feeq_size(const al_feeq* q) {
    return q == nullptr ? 0 : static_cast<uint64_t>(q->queue.size());
}

int32_t al_feeq_remove_txids(al_feeq* q, const uint8_t* txids, uint32_t n) {
    if (q == nullptr || (txids == nullptr && n != 0)) {
        return AL_ERR_ARG;
    }
    return guarded([&]() -> int32_t {
        std::vector<Hash256> ids(n);
        for (uint32_t i = 0; i < n; ++i) {
            std::memcpy(ids[i].data(), txids + static_cast<std::size_t>(i) * 32, 32);
        }
        q->queue.remove_txids(ids);
        return AL_OK;
    });
}

} // extern "C"
