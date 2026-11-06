#include "pow_c.h"

#include "aletheia/pow.h"

#include <cstring>

using namespace aletheia;
using aletheia::capi::guarded;

static_assert(AL_POW_MAX_DIFFICULTY == MAX_DIFFICULTY, "C and C++ limits must agree");

extern "C" {

int32_t al_pow_search(const uint8_t header[88], uint64_t start, uint64_t count,
                      uint32_t difficulty, uint64_t* out_nonce, uint8_t out_hash[32]) {
    if (header == nullptr || out_nonce == nullptr || out_hash == nullptr ||
        difficulty > AL_POW_MAX_DIFFICULTY) {
        return AL_ERR_ARG;
    }
    return guarded([&]() -> int32_t {
        const PowResult r = search_nonces(header, start, count, difficulty);
        if (!r.found) {
            return 0;
        }
        *out_nonce = r.nonce;
        std::memcpy(out_hash, r.hash.data(), 32);
        return 1;
    });
}

int32_t al_pow_check(const uint8_t hash[32], uint32_t difficulty) {
    if (hash == nullptr || difficulty > AL_POW_MAX_DIFFICULTY) {
        return AL_ERR_ARG;
    }
    return guarded([&]() -> int32_t {
        Hash256 h;
        std::memcpy(h.data(), hash, 32);
        return meets_difficulty(h, difficulty) ? 1 : 0;
    });
}

} // extern "C"
