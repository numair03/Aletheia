// Tests for the open-addressing hash map (cpp/include/aletheia/hash_map.h).

#include "aletheia/hash_map.h"
#include "aletheia/sha256.h"

#include "check.h"

#include <cstdint>
#include <random>
#include <type_traits>
#include <string>
#include <vector>

using aletheia::Hash256;
using aletheia::Hash256Hasher;
using aletheia::HashMap;
using aletheia::SHA256;

namespace {

struct IdentityHasher {
    std::uint64_t operator()(std::uint64_t k) const { return k; }
};

// Every key lands in the same home slot, so every operation must probe.
struct CollidingHasher {
    std::uint64_t operator()(std::uint64_t) const { return 3; }
};

template <typename Map>
bool load_ok(const Map& m) {
    return (m.size() + m.tombstones()) * 10 <= m.capacity() * 7;
}

void test_basic() {
    HashMap<std::uint64_t, std::string, IdentityHasher> m;
    CHECK(m.empty());
    CHECK(m.find(1) == nullptr);
    CHECK(!m.erase(1));

    CHECK(m.insert_or_assign(1, "one"));
    CHECK(m.insert_or_assign(2, "two"));
    CHECK_EQ(m.size(), 2u);
    CHECK(m.contains(1));
    CHECK(m.find(2) != nullptr && *m.find(2) == "two");

    // Overwrite: no new entry.
    CHECK(!m.insert_or_assign(1, "uno"));
    CHECK_EQ(m.size(), 2u);
    CHECK_EQ(*m.find(1), std::string("uno"));

    // Mutation through find().
    *m.find(2) = "dos";
    CHECK_EQ(*m.find(2), std::string("dos"));

    CHECK(m.erase(1));
    CHECK(!m.contains(1));
    CHECK(!m.erase(1));
    CHECK_EQ(m.size(), 1u);
    CHECK_EQ(m.tombstones(), 1u);

    m.clear();
    CHECK(m.empty());
    CHECK_EQ(m.tombstones(), 0u);
    CHECK(!m.contains(2));
}

void test_capacity_rounding() {
    CHECK_EQ((HashMap<std::uint64_t, int, IdentityHasher>(0).capacity()), 8u);
    CHECK_EQ((HashMap<std::uint64_t, int, IdentityHasher>(17).capacity()), 32u);
    CHECK_EQ((HashMap<std::uint64_t, int, IdentityHasher>(64).capacity()), 64u);
}

// The case tombstones exist for: erasing a key in the middle of a probe chain
// must not hide the keys stored after it.
void test_tombstone_keeps_chain() {
    HashMap<std::uint64_t, int, CollidingHasher> m(16);
    for (std::uint64_t k = 0; k < 5; ++k) m.insert_or_assign(k, static_cast<int>(k * 10));
    CHECK(m.erase(2)); // middle of the chain 0,1,2,3,4
    CHECK(m.find(3) != nullptr && *m.find(3) == 30);
    CHECK(m.find(4) != nullptr && *m.find(4) == 40);
    CHECK(!m.contains(2));

    // Re-inserting reuses the tombstone instead of extending the chain.
    CHECK(m.insert_or_assign(7, 70));
    CHECK_EQ(m.tombstones(), 0u);
    CHECK_EQ(*m.find(7), 70);

    // Inserting a key that exists *after* a tombstone must overwrite it, not
    // create a duplicate in the tombstone's slot.
    CHECK(m.erase(1));
    CHECK(!m.insert_or_assign(4, 44));
    CHECK_EQ(m.size(), 4u);
    CHECK_EQ(*m.find(4), 44);
    CHECK(m.erase(4));
    CHECK(!m.contains(4)); // would still be found if a duplicate existed
}

void test_growth_preserves_entries() {
    HashMap<std::uint64_t, std::uint64_t, IdentityHasher> m(8);
    const std::size_t start = m.capacity();
    bool always_ok = true;
    for (std::uint64_t k = 0; k < 10000; ++k) {
        m.insert_or_assign(k * 7919, k);
        always_ok = always_ok && load_ok(m);
    }
    CHECK(always_ok);
    CHECK(m.capacity() > start);
    CHECK_EQ(m.size(), 10000u);
    bool all_found = true;
    for (std::uint64_t k = 0; k < 10000; ++k) {
        const std::uint64_t* v = m.find(k * 7919);
        all_found = all_found && v != nullptr && *v == k;
    }
    CHECK(all_found);
}

// Insert/erase churn at a constant live size: tombstones must be recycled by
// same-size rebuilds, not cause unbounded growth.
void test_churn_does_not_grow() {
    HashMap<std::uint64_t, int, IdentityHasher> m(64);
    for (std::uint64_t k = 0; k < 10; ++k) m.insert_or_assign(k, 0);
    bool always_ok = true;
    for (std::uint64_t k = 10; k < 100000; ++k) {
        m.insert_or_assign(k, 0);
        m.erase(k - 10);
        always_ok = always_ok && load_ok(m);
    }
    CHECK(always_ok);
    CHECK_EQ(m.size(), 10u);
    CHECK_EQ(m.capacity(), 64u);
}

// Random operations checked against a plain array indexed by key.
template <typename Hasher>
void stress(std::uint32_t seed) {
    constexpr std::uint64_t KEYS = 2000;
    HashMap<std::uint64_t, std::uint64_t, Hasher> m;
    std::vector<bool> present(KEYS, false);
    std::vector<std::uint64_t> value(KEYS, 0);
    std::size_t expected_size = 0;
    std::mt19937_64 rng(seed);

    bool ok = true;
    for (int step = 0; step < (std::is_same<Hasher, CollidingHasher>::value ? 6000 : 200000); ++step) {
        const std::uint64_t k = rng() % KEYS;
        switch (rng() % 3) {
        case 0: { // insert or assign
            const std::uint64_t v = rng();
            const bool inserted = m.insert_or_assign(k, v);
            ok = ok && inserted == !present[k];
            if (!present[k]) ++expected_size;
            present[k] = true;
            value[k] = v;
            break;
        }
        case 1: { // erase
            const bool erased = m.erase(k);
            ok = ok && erased == present[k];
            if (present[k]) --expected_size;
            present[k] = false;
            break;
        }
        default: { // lookup
            const std::uint64_t* v = m.find(k);
            ok = ok && (v != nullptr) == present[k];
            if (v != nullptr && present[k]) ok = ok && *v == value[k];
        }
        }
        ok = ok && m.size() == expected_size && load_ok(m);
    }
    CHECK(ok);

    // Final full comparison, including for_each visiting each entry once.
    std::size_t visited = 0;
    bool each_ok = true;
    m.for_each([&](std::uint64_t k, std::uint64_t v) {
        ++visited;
        each_ok = each_ok && k < KEYS && present[k] && value[k] == v;
    });
    CHECK(each_ok);
    CHECK_EQ(visited, expected_size);
}

void test_hash256_keys() {
    HashMap<Hash256, std::uint64_t, Hash256Hasher> m;
    std::vector<Hash256> keys;
    for (std::uint32_t i = 0; i < 5000; ++i) {
        const std::string s = "account-" + std::to_string(i);
        keys.push_back(SHA256::hash(reinterpret_cast<const std::uint8_t*>(s.data()), s.size()));
        m.insert_or_assign(keys.back(), i);
    }
    CHECK_EQ(m.size(), 5000u);
    bool all = true;
    for (std::uint32_t i = 0; i < keys.size(); ++i) {
        const std::uint64_t* v = m.find(keys[i]);
        all = all && v != nullptr && *v == i;
    }
    CHECK(all);

    // Keys differing in a single byte (non-SHA account IDs) are distinct.
    Hash256 a{}, b{};
    b[31] = 1;
    HashMap<Hash256, int, Hash256Hasher> small;
    small.insert_or_assign(a, 1);
    small.insert_or_assign(b, 2);
    CHECK_EQ(*small.find(a), 1);
    CHECK_EQ(*small.find(b), 2);
    CHECK(Hash256Hasher{}(a) != Hash256Hasher{}(b));
}

void test_copy_is_independent() {
    HashMap<std::uint64_t, int, IdentityHasher> a;
    a.insert_or_assign(1, 10);
    HashMap<std::uint64_t, int, IdentityHasher> b = a;
    b.insert_or_assign(1, 11);
    b.insert_or_assign(2, 20);
    CHECK_EQ(*a.find(1), 10);
    CHECK(!a.contains(2));
    CHECK_EQ(*b.find(1), 11);
}

} // namespace

int main() {
    test_basic();
    test_capacity_rounding();
    test_tombstone_keeps_chain();
    test_growth_preserves_entries();
    test_churn_does_not_grow();
    stress<IdentityHasher>(1);
    stress<IdentityHasher>(2);
    stress<CollidingHasher>(3);
    test_hash256_keys();
    test_copy_is_independent();
    return aletheia_test::finish("test_hash_map");
}
