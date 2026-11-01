#pragma once

#include "aletheia/types.h"

#include <cstddef>
#include <cstdint>
#include <vector>

namespace aletheia {

// SHA-256 as specified in FIPS PUB 180-4, written from scratch. Correctness is
// checked against published and independently computed digests in
// cpp/tests/test_crypto.cpp.
//
// Usage: update() any number of times, then finalize(). finalize() returns the
// digest and resets the object, so it can immediately hash a new message.
class SHA256 {
public:
    SHA256();
    void reset();
    // `data` may be null only when `len` is 0.
    void update(const uint8_t* data, size_t len);
    void update(const std::vector<uint8_t>& data);
    Hash256 finalize();

    // Convenience one-shot hashing methods
    static Hash256 hash(const uint8_t* data, size_t len);
    static Hash256 hash(const std::vector<uint8_t>& data);
    static Hash256 hash(const Hash256& left, const Hash256& right);

private:
    void transform(const uint8_t* chunk);

    uint32_t state_[8];
    uint64_t bit_count_;
    uint8_t buffer_[64];
};

} // namespace aletheia
