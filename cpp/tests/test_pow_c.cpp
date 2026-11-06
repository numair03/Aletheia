// Tests for the proof-of-work C API (pow_c.h): argument checks, untouched
// outputs, agreement with the C++ functions, and agreement with D1's header
// encoding and hash through the ledger C API.

#include "pow_c.h"

#include "aletheia/block.h"
#include "aletheia/pow.h"
#include "check.h"
#include "ledger_c.h"

#include <cstdint>
#include <cstring>
#include <limits>
#include <random>

using namespace aletheia;

namespace {

constexpr std::uint64_t U64_MAX = std::numeric_limits<std::uint64_t>::max();
constexpr std::uint64_t SENTINEL = 0xdeadbeefcafef00dull;

std::mt19937_64 rng(1106);

al_header random_header() {
    al_header h{};
    h.version = static_cast<std::uint32_t>(rng());
    for (auto& b : h.prev) b = static_cast<std::uint8_t>(rng());
    for (auto& b : h.merkle_root) b = static_cast<std::uint8_t>(rng());
    h.timestamp = static_cast<std::int64_t>(rng());
    h.difficulty = static_cast<std::uint32_t>(rng());
    h.nonce = rng();
    return h;
}

void encode_c(const al_header& h, std::uint8_t out[88]) {
    CHECK_EQ(al_header_encode(&h, out), AL_OK);
}

void test_arguments() {
    const al_header h = random_header();
    std::uint8_t enc[88];
    encode_c(h, enc);
    std::uint64_t nonce = SENTINEL;
    std::uint8_t hash[32];
    std::memset(hash, 0xab, 32);

    CHECK_EQ(al_pow_search(nullptr, 0, 10, 0, &nonce, hash), AL_ERR_ARG);
    CHECK_EQ(al_pow_search(enc, 0, 10, 0, nullptr, hash), AL_ERR_ARG);
    CHECK_EQ(al_pow_search(enc, 0, 10, 0, &nonce, nullptr), AL_ERR_ARG);
    CHECK_EQ(al_pow_search(enc, 0, 10, AL_POW_MAX_DIFFICULTY + 1, &nonce, hash), AL_ERR_ARG);
    CHECK_EQ(al_pow_search(enc, 0, 10, std::numeric_limits<std::uint32_t>::max(), &nonce, hash),
             AL_ERR_ARG);
    // Rejected calls write nothing.
    CHECK_EQ(nonce, SENTINEL);
    bool untouched = true;
    for (std::uint8_t b : hash) untouched = untouched && b == 0xab;
    CHECK(untouched);

    // The maximum difficulty is a valid argument (and is not met here).
    CHECK_EQ(al_pow_search(enc, 0, 100, AL_POW_MAX_DIFFICULTY, &nonce, hash), 0);
    CHECK_EQ(nonce, SENTINEL);

    // count == 0 is not an error: nothing is tried.
    CHECK_EQ(al_pow_search(enc, 7, 0, 0, &nonce, hash), 0);
    CHECK_EQ(nonce, SENTINEL);

    std::uint8_t zero[32] = {0};
    CHECK_EQ(al_pow_check(nullptr, 0), AL_ERR_ARG);
    CHECK_EQ(al_pow_check(zero, AL_POW_MAX_DIFFICULTY + 1), AL_ERR_ARG);
    CHECK_EQ(al_pow_check(zero, AL_POW_MAX_DIFFICULTY), 1);
    CHECK_EQ(al_pow_check(zero, 0), 1);
}

// The C search returns exactly what the C++ search returns, and the hash it
// reports is D1's header hash with that nonce (al_header_hash).
void test_search_agreement() {
    for (int t = 0; t < 40; ++t) {
        const al_header h = random_header();
        std::uint8_t enc[88];
        encode_c(h, enc);
        const std::uint32_t d = static_cast<std::uint32_t>(rng() % 11); // 0..10
        const std::uint64_t start = (t % 3 == 0) ? U64_MAX - (rng() % 50) : rng();
        const std::uint64_t count = rng() % 5000;

        std::uint64_t nonce = SENTINEL;
        std::uint8_t hash[32] = {0};
        const std::int32_t st = al_pow_search(enc, start, count, d, &nonce, hash);
        const PowResult want = search_nonces(enc, start, count, d);
        CHECK(st == 0 || st == 1);
        CHECK_EQ(st == 1, want.found);
        if (st == 1) {
            CHECK_EQ(nonce, want.nonce);
            CHECK(std::memcmp(hash, want.hash.data(), 32) == 0);
            al_header solved = h;
            solved.nonce = nonce;
            std::uint8_t d1[32];
            CHECK_EQ(al_header_hash(&solved, d1), AL_OK);
            CHECK(std::memcmp(hash, d1, 32) == 0);
            CHECK_EQ(al_pow_check(d1, d), 1);
        } else {
            CHECK_EQ(nonce, SENTINEL); // untouched when not found
        }
    }
}

void test_wrap() {
    const al_header h = random_header();
    std::uint8_t enc[88];
    encode_c(h, enc);
    std::uint64_t nonce = 0;
    std::uint8_t hash[32];
    CHECK_EQ(al_pow_search(enc, U64_MAX, U64_MAX, 0, &nonce, hash), 1);
    CHECK_EQ(nonce, U64_MAX);
}

void test_check_agreement() {
    for (int t = 0; t < 300; ++t) {
        Hash256 h;
        for (auto& b : h) b = static_cast<std::uint8_t>(rng());
        const std::size_t z = rng() % 5;
        for (std::size_t k = 0; k < z; ++k) h[k] = 0;
        for (std::uint32_t d = 0; d <= AL_POW_MAX_DIFFICULTY; d += 1 + static_cast<std::uint32_t>(rng() % 7)) {
            CHECK_EQ(al_pow_check(h.data(), d), meets_difficulty(h, d) ? 1 : 0);
        }
        const std::uint32_t lz = leading_zero_bits(h);
        CHECK_EQ(al_pow_check(h.data(), lz), 1);
        CHECK_EQ(al_pow_check(h.data(), lz + 1), 0);
    }
}

} // namespace

int main() {
    test_arguments();
    test_search_agreement();
    test_wrap();
    test_check_agreement();
    return aletheia_test::finish("test_pow_c");
}
