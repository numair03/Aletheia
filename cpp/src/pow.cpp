#include "aletheia/pow.h"

#include "aletheia/sha256.h"

#include <cstring>
#include <limits>

namespace aletheia {

std::uint32_t leading_zero_bits(const Hash256& h) {
    std::uint32_t n = 0;
    for (std::uint8_t byte : h) {
        if (byte == 0) {
            n += 8;
            continue;
        }
        // The first non-zero byte: count its zero bits from the top.
        for (int bit = 7; bit >= 0 && ((byte >> bit) & 1u) == 0; --bit) {
            ++n;
        }
        break;
    }
    return n;
}

bool meets_difficulty(const Hash256& h, std::uint32_t difficulty) {
    return difficulty <= MAX_DIFFICULTY && leading_zero_bits(h) >= difficulty;
}

PowResult search_nonces(const std::uint8_t* header, std::uint64_t start,
                        std::uint64_t count, std::uint32_t difficulty) {
    PowResult result;
    if (count == 0 || difficulty > MAX_DIFFICULTY) {
        return result;
    }
    std::uint8_t buf[HEADER_SIZE];
    std::memcpy(buf, header, HEADER_SIZE);

    // Last nonce to try, clamped so that start + count never wraps past 2^64-1.
    const std::uint64_t max = std::numeric_limits<std::uint64_t>::max();
    const std::uint64_t last = (count - 1 > max - start) ? max : start + (count - 1);

    for (std::uint64_t n = start;; ++n) {
        for (int i = 0; i < 8; ++i) {
            buf[NONCE_OFFSET + i] = static_cast<std::uint8_t>(n >> (56 - 8 * i));
        }
        const Hash256 h = SHA256::hash(buf, HEADER_SIZE);
        if (leading_zero_bits(h) >= difficulty) {
            result.found = true;
            result.nonce = n;
            result.hash = h;
            return result;
        }
        if (n == last) {
            return result; // also stops before ++n could wrap to 0
        }
    }
}

} // namespace aletheia
