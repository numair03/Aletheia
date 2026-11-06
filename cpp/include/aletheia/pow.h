#pragma once

#include "aletheia/block.h"
#include "aletheia/types.h"

#include <cstddef>
#include <cstdint>

namespace aletheia {

// Proof of work. A header meets difficulty d when SHA-256 of its canonical
// encoding (the block hash) starts with at least d zero bits, reading the
// hash as a big-endian bit string (bit 7 of byte 0 first).
//
// The nonce search is a pure function over one bounded range of nonces: no
// shared state, no allocation, no cancellation. Go splits the nonce space
// into chunks, runs one search per worker thread, and stops between chunks
// (internal/miner). Everything here is therefore safe to call from several
// threads at once.

// The largest meaningful difficulty: a SHA-256 hash has 256 bits.
constexpr std::uint32_t MAX_DIFFICULTY = 256;

// The nonce is the last field of the canonical header encoding (block.h):
// version 4 + prev 32 + merkle_root 32 + timestamp 8 + difficulty 4 = 80.
constexpr std::size_t NONCE_OFFSET = 80;
static_assert(NONCE_OFFSET + 8 == HEADER_SIZE, "nonce must be the last 8 header bytes");

// Number of leading zero bits of `h` (0..256).
std::uint32_t leading_zero_bits(const Hash256& h);

// True if `h` has at least `difficulty` leading zero bits. Always true for
// difficulty 0; always false above MAX_DIFFICULTY.
bool meets_difficulty(const Hash256& h, std::uint32_t difficulty);

struct PowResult {
    bool found = false;
    std::uint64_t nonce = 0; // valid only if found
    Hash256 hash{};          // the block hash for `nonce`; valid only if found
};

// Tries the nonces start, start+1, ..., start+count-1 in increasing order,
// writing each big-endian into bytes 80..87 of a copy of `header` (an
// encoded header of HEADER_SIZE bytes; its own nonce bytes are ignored), and
// returns the first nonce whose hash meets `difficulty`. The range never
// wraps: the search stops after nonce 2^64-1. count == 0 finds nothing.
PowResult search_nonces(const std::uint8_t* header, std::uint64_t start,
                        std::uint64_t count, std::uint32_t difficulty);

} // namespace aletheia
