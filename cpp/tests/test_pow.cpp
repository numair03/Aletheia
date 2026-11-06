// Tests for the proof-of-work primitives (pow.h): leading-zero-bit count,
// the difficulty predicate and the chunked nonce search. The search is
// checked against an independent reference: D1's canonical header encoding
// and header_hash with the nonce set field by field, scanned one nonce at a
// time.

#include "aletheia/block.h"
#include "aletheia/pow.h"
#include "aletheia/sha256.h"
#include "check.h"

#include <cstdint>
#include <cstring>
#include <limits>
#include <random>
#include <thread>
#include <vector>

using namespace aletheia;

namespace {

constexpr std::uint64_t U64_MAX = std::numeric_limits<std::uint64_t>::max();

std::mt19937_64 rng(20261106);

// Reference count: walk the 256 bits one by one, most significant first.
std::uint32_t ref_lzb(const Hash256& h) {
    std::uint32_t n = 0;
    for (std::uint32_t i = 0; i < 256; ++i) {
        if ((h[i / 8] >> (7 - i % 8)) & 1u) break;
        ++n;
    }
    return n;
}

Hash256 random_hash() {
    Hash256 h;
    for (auto& b : h) b = static_cast<std::uint8_t>(rng());
    return h;
}

BlockHeader random_header() {
    BlockHeader h;
    h.version = static_cast<std::uint32_t>(rng());
    h.prev = random_hash();
    h.merkle_root = random_hash();
    h.timestamp = static_cast<std::int64_t>(rng());
    h.difficulty = static_cast<std::uint32_t>(rng());
    h.nonce = rng(); // must be ignored by the search
    return h;
}

// Reference block hash for `h` with nonce `n`, via D1's encoding.
Hash256 ref_hash(BlockHeader h, std::uint64_t n) {
    h.nonce = n;
    return header_hash(h);
}

// Reference search: first n in [start, start+count) (no wrap) meeting d.
PowResult ref_search(const BlockHeader& h, std::uint64_t start, std::uint64_t count,
                     std::uint32_t d) {
    PowResult r;
    for (std::uint64_t i = 0; i < count; ++i) {
        const std::uint64_t n = start + i;
        const Hash256 hh = ref_hash(h, n);
        if (ref_lzb(hh) >= d) {
            r.found = true;
            r.nonce = n;
            r.hash = hh;
            return r;
        }
        if (n == U64_MAX) break;
    }
    return r;
}

PowResult search(const BlockHeader& h, std::uint64_t start, std::uint64_t count,
                 std::uint32_t d) {
    const HeaderBytes enc = encode(h);
    return search_nonces(enc.data(), start, count, d);
}

bool same(const PowResult& a, const PowResult& b) {
    if (a.found != b.found) return false;
    return !a.found || (a.nonce == b.nonce && a.hash == b.hash);
}

void test_leading_zero_bits() {
    Hash256 zero{};
    CHECK_EQ(leading_zero_bits(zero), 256u);

    Hash256 ones;
    ones.fill(0xff);
    CHECK_EQ(leading_zero_bits(ones), 0u);

    // Exactly one bit set at position p (0 = most significant): p zeros.
    // With every lower bit also set the answer must not change.
    for (std::uint32_t p = 0; p < 256; ++p) {
        Hash256 h{};
        h[p / 8] = static_cast<std::uint8_t>(0x80u >> (p % 8));
        CHECK_EQ(leading_zero_bits(h), p);
        for (std::uint32_t q = p + 1; q < 256; ++q) {
            h[q / 8] |= static_cast<std::uint8_t>(0x80u >> (q % 8));
        }
        CHECK_EQ(leading_zero_bits(h), p);
    }

    // Crafted bytes around the byte boundaries.
    Hash256 a{};
    a[0] = 0x01;
    CHECK_EQ(leading_zero_bits(a), 7u);
    a[0] = 0x00;
    a[1] = 0x01;
    CHECK_EQ(leading_zero_bits(a), 15u);
    a[1] = 0x00;
    a[2] = 0x80;
    CHECK_EQ(leading_zero_bits(a), 16u);

    for (int i = 0; i < 2000; ++i) {
        Hash256 h = random_hash();
        // Zero a random prefix so that long counts occur too.
        const std::size_t z = rng() % 33;
        for (std::size_t k = 0; k < z; ++k) h[k] = 0;
        CHECK_EQ(leading_zero_bits(h), ref_lzb(h));
    }
}

void test_meets_difficulty() {
    Hash256 zero{};
    CHECK(meets_difficulty(zero, 0));
    CHECK(meets_difficulty(zero, 256));
    CHECK(!meets_difficulty(zero, 257));
    CHECK(!meets_difficulty(zero, std::numeric_limits<std::uint32_t>::max()));

    Hash256 ones;
    ones.fill(0xff);
    CHECK(meets_difficulty(ones, 0));
    CHECK(!meets_difficulty(ones, 1));

    for (int i = 0; i < 500; ++i) {
        Hash256 h = random_hash();
        const std::size_t z = rng() % 4;
        for (std::size_t k = 0; k < z; ++k) h[k] = 0;
        const std::uint32_t lz = ref_lzb(h);
        // The boundary exactly: d <= lz meets, d = lz + 1 does not.
        CHECK(meets_difficulty(h, lz));
        CHECK(!meets_difficulty(h, lz + 1));
        for (std::uint32_t d = 0; d <= 258; d += 1 + static_cast<std::uint32_t>(rng() % 9)) {
            CHECK_EQ(meets_difficulty(h, d), d <= lz);
        }
    }
}

// The search finds exactly the reference's first solution in every range,
// and the hash it returns is D1's header hash for that nonce.
void test_search_matches_reference() {
    for (int t = 0; t < 12; ++t) {
        const BlockHeader h = random_header();
        const std::uint32_t d = 5 + static_cast<std::uint32_t>(t % 4); // 5..8
        const std::uint64_t base = (t % 2 == 0) ? 0 : (rng() >> 1);
        const std::uint64_t span = 3000;

        // Every solution in [base, base + span), by the reference.
        std::vector<std::uint64_t> sols;
        for (std::uint64_t i = 0; i < span; ++i) {
            if (ref_lzb(ref_hash(h, base + i)) >= d) sols.push_back(base + i);
        }
        CHECK(!sols.empty());

        for (int q = 0; q < 60; ++q) {
            const std::uint64_t s = base + rng() % span;
            const std::uint64_t c = rng() % (base + span - s + 1);
            const PowResult got = search(h, s, c, d);
            const PowResult want = ref_search(h, s, c, d);
            CHECK(same(got, want));
            if (got.found) {
                CHECK(got.nonce >= s && got.nonce - s < c);
                CHECK(meets_difficulty(got.hash, d));
                CHECK(got.hash == ref_hash(h, got.nonce));
            }
        }

        // Range edges around a known solution x: [x, x+1) finds it,
        // [x-k, x) misses it, and a range ending at x includes it.
        const std::uint64_t x = sols.back();
        PowResult r = search(h, x, 1, d);
        CHECK(r.found && r.nonce == x);
        r = search(h, x + 1, 0, d);
        CHECK(!r.found);
        if (sols.size() >= 2) {
            const std::uint64_t prev = sols[sols.size() - 2];
            r = search(h, prev + 1, x - prev - 1, d); // strictly between two solutions
            CHECK(!r.found);
            r = search(h, prev + 1, x - prev, d); // ends exactly at x
            CHECK(r.found && r.nonce == x);
        }
    }
}

// Searching a range in one call or in consecutive chunks of any size gives
// the same first solution: chunk boundaries neither skip nor repeat a nonce.
void test_chunk_boundaries() {
    for (int t = 0; t < 6; ++t) {
        const BlockHeader h = random_header();
        const std::uint32_t d = 9;
        const std::uint64_t total = 4096;
        const PowResult whole = search(h, 0, total, d);
        CHECK(same(whole, ref_search(h, 0, total, d)));
        for (std::uint64_t chunk : {1ull, 2ull, 7ull, 64ull, 1000ull, 4095ull}) {
            PowResult first;
            for (std::uint64_t s = 0; s < total && !first.found; s += chunk) {
                const std::uint64_t c = (total - s < chunk) ? total - s : chunk;
                first = search(h, s, c, d);
            }
            CHECK(same(first, whole));
        }
    }
}

// A realistic difficulty with the real chunk size (65,536): the first
// solution found by consecutive chunks equals the reference's first solution
// from 0, even when it lies beyond the first chunk.
void test_known_nonce_real_chunks() {
    constexpr std::uint64_t C = 1u << 16;
    const std::uint32_t d = 17;
    int beyond_first_chunk = 0;
    for (int t = 0; t < 8; ++t) {
        const BlockHeader h = random_header();
        const PowResult want = ref_search(h, 0, 64 * C, d);
        CHECK(want.found);
        PowResult got;
        for (std::uint64_t s = 0; !got.found && s < 64 * C; s += C) {
            got = search(h, s, C, d);
        }
        CHECK(same(got, want));
        if (want.nonce >= C) ++beyond_first_chunk;
    }
    // Each first solution lies beyond C with probability ~0.6 (seeded, so
    // deterministic); this guarantees that a chunk boundary was crossed.
    CHECK(beyond_first_chunk > 0);
}

void test_degenerate_arguments() {
    const BlockHeader h = random_header();
    const HeaderBytes enc = encode(h);

    // count == 0: nothing is tried, even at difficulty 0.
    PowResult r = search_nonces(enc.data(), 42, 0, 0);
    CHECK(!r.found);

    // Difficulty 0: the first nonce of the range, with its real hash.
    r = search_nonces(enc.data(), 42, 5, 0);
    CHECK(r.found && r.nonce == 42 && r.hash == ref_hash(h, 42));

    // Difficulty above 256 can never be met; it returns at once.
    r = search_nonces(enc.data(), 0, U64_MAX, 257);
    CHECK(!r.found);

    // The header's own nonce bytes are ignored.
    BlockHeader h2 = h;
    h2.nonce = ~h.nonce;
    const HeaderBytes enc2 = encode(h2);
    for (std::uint32_t d = 0; d <= 8; d += 4) {
        CHECK(same(search_nonces(enc.data(), 0, 2000, d), search_nonces(enc2.data(), 0, 2000, d)));
    }
}

// Near 2^64 the range is clamped: the search stops after nonce 2^64-1 and
// never wraps around to nonce 0.
void test_wrap_guard() {
    // Difficulty 0 finds the first nonce, at the very top too.
    BlockHeader h = random_header();
    PowResult r = search(h, U64_MAX, 1, 0);
    CHECK(r.found && r.nonce == U64_MAX && r.hash == ref_hash(h, U64_MAX));
    r = search(h, U64_MAX, U64_MAX, 0); // count far past the end
    CHECK(r.found && r.nonce == U64_MAX);
    r = search(h, 5, U64_MAX, 0); // start + count - 1 overflows
    CHECK(r.found && r.nonce == 5);

    // A header whose nonce 0 meets d but whose top three nonces do not: a
    // wrapping search over [2^64-3, 2^64+7) would return 0.
    bool tested = false;
    for (int t = 0; t < 4000 && !tested; ++t) {
        h = random_header();
        const std::uint32_t lz0 = ref_lzb(ref_hash(h, 0));
        std::uint32_t top = 0;
        for (std::uint64_t n = U64_MAX - 2;; ++n) {
            const std::uint32_t lz = ref_lzb(ref_hash(h, n));
            if (lz > top) top = lz;
            if (n == U64_MAX) break;
        }
        if (lz0 > top) {
            const std::uint32_t d = lz0;
            r = search(h, U64_MAX - 2, 10, d);
            CHECK(!r.found);
            r = search(h, 0, 1, d);
            CHECK(r.found && r.nonce == 0);
            tested = true;
        }
    }
    CHECK(tested);
}

// The search is a pure function: concurrent searches from several threads
// give the same results as sequential ones.
void test_concurrent_searches() {
    constexpr int T = 4;
    std::vector<BlockHeader> hs;
    std::vector<PowResult> want;
    for (int i = 0; i < T; ++i) {
        hs.push_back(random_header());
        want.push_back(search(hs.back(), 0, 1u << 15, 12));
    }
    std::vector<PowResult> got(T);
    std::vector<std::thread> threads;
    for (int i = 0; i < T; ++i) {
        threads.emplace_back([&, i] { got[i] = search(hs[i], 0, 1u << 15, 12); });
    }
    for (auto& th : threads) th.join();
    for (int i = 0; i < T; ++i) CHECK(same(got[i], want[i]));
}

} // namespace

int main() {
    test_leading_zero_bits();
    test_meets_difficulty();
    test_search_matches_reference();
    test_chunk_boundaries();
    test_known_nonce_real_chunks();
    test_degenerate_arguments();
    test_wrap_guard();
    test_concurrent_searches();
    return aletheia_test::finish("test_pow");
}
