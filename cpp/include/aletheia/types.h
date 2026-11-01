#pragma once

#include <array>
#include <cstdint>
#include <iomanip>
#include <sstream>
#include <string>
#include <vector>
#include <cstddef>
#include <optional>

namespace aletheia {

// Standard 256-bit cryptographic hash representation (32 bytes)
using Hash256 = std::array<std::uint8_t, 32>;

// Sentinel zero-hash constant
inline constexpr Hash256 ZERO_HASH = {0};

// Relative direction of a sibling node in a Merkle inclusion proof
enum class ProofDirection : uint8_t {
    Left = 0,   // Sibling is on the left:  hash(sibling, current)
    Right = 1   // Sibling is on the right: hash(current, sibling)
};

// Single step in an SPV Merkle inclusion proof
struct MerkleStep {
    Hash256 hash;
    ProofDirection direction;
};

// Complete SPV Merkle proof for a specific leaf index
struct MerkleProof {
    size_t leaf_index;
    std::vector<MerkleStep> steps;
};

// Convert a 32-byte hash to a lowercase hex string
inline std::string to_hex(const Hash256& hash) {
    std::ostringstream oss;
    oss << std::hex << std::setfill('0');
    for (uint8_t byte : hash) {
        oss << std::setw(2) << static_cast<int>(byte);
    }
    return oss.str();
}

// Parse exactly 64 hexadecimal characters (either case) into a 32-byte hash.
// Returns std::nullopt for any other input: wrong length, or any non-hex
// character (including sign, whitespace, or a "0x" prefix). Never throws.
// A valid all-zero string returns a value, so failure is never confused with
// a zero hash.
inline std::optional<Hash256> from_hex(const std::string& hex_str) {
    if (hex_str.size() != 64) {
        return std::nullopt;
    }
    const auto nibble = [](char c) -> int {
        if (c >= '0' && c <= '9') return c - '0';
        if (c >= 'a' && c <= 'f') return c - 'a' + 10;
        if (c >= 'A' && c <= 'F') return c - 'A' + 10;
        return -1;
    };
    Hash256 hash{};
    for (std::size_t i = 0; i < 32; ++i) {
        const int hi = nibble(hex_str[2 * i]);
        const int lo = nibble(hex_str[2 * i + 1]);
        if (hi < 0 || lo < 0) {
            return std::nullopt;
        }
        hash[i] = static_cast<std::uint8_t>((hi << 4) | lo);
    }
    return hash;
}

} // namespace aletheia
