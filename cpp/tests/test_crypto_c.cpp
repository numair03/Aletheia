// Tests for the crypto C API (crypto_c.h): results must match the C++ classes
// exactly, argument errors must return status codes, and nothing may throw.

#include "crypto_c.h"

#include "aletheia/merkle.h"
#include "aletheia/sha256.h"
#include "check.h"

#include <cstring>
#include <vector>

using namespace aletheia;

namespace {

std::vector<Hash256> leaves(size_t n) {
    std::vector<Hash256> out;
    for (size_t i = 0; i < n; ++i) {
        const uint8_t b = static_cast<uint8_t>(i);
        out.push_back(SHA256::hash(&b, 1));
    }
    return out;
}

std::vector<uint8_t> flatten(const std::vector<Hash256>& v) {
    std::vector<uint8_t> out(v.size() * 32);
    for (size_t i = 0; i < v.size(); ++i) std::memcpy(out.data() + 32 * i, v[i].data(), 32);
    return out;
}

bool equals(const uint8_t* a, const Hash256& b) { return std::memcmp(a, b.data(), 32) == 0; }

void test_sha256() {
    uint8_t out[32];
    const uint8_t abc[] = {'a', 'b', 'c'};
    CHECK_EQ(al_sha256(abc, 3, out), AL_OK);
    CHECK(equals(out, SHA256::hash(abc, 3)));
    CHECK_EQ(al_sha256(nullptr, 0, out), AL_OK); // empty message
    CHECK(equals(out, SHA256::hash(nullptr, 0)));
    CHECK_EQ(al_sha256(nullptr, 1, out), AL_ERR_ARG);
    CHECK_EQ(al_sha256(abc, 3, nullptr), AL_ERR_ARG);
}

void test_merkle_root() {
    uint8_t out[32];
    for (size_t n = 0; n <= 17; ++n) {
        const std::vector<Hash256> l = leaves(n);
        const std::vector<uint8_t> flat = flatten(l);
        CHECK_EQ(al_merkle_root(n ? flat.data() : nullptr, n, out), AL_OK);
        CHECK(equals(out, MerkleTree(l).root()));
    }
    CHECK_EQ(al_merkle_root(nullptr, 3, out), AL_ERR_ARG);
    const std::vector<uint8_t> flat = flatten(leaves(2));
    CHECK_EQ(al_merkle_root(flat.data(), 2, nullptr), AL_ERR_ARG);
    // An n whose byte size cannot be represented is rejected before any read.
    CHECK_EQ(al_merkle_root(flat.data(), UINT64_MAX, out), AL_ERR_ARG);
}

void test_merkle_proof_and_verify() {
    for (size_t n = 1; n <= 17; ++n) {
        const std::vector<Hash256> l = leaves(n);
        const std::vector<uint8_t> flat = flatten(l);
        const Hash256 root = MerkleTree(l).root();
        for (size_t i = 0; i < n; ++i) {
            uint8_t hashes[32 * AL_MERKLE_MAX_STEPS];
            uint8_t dirs[AL_MERKLE_MAX_STEPS];
            uint32_t steps = 999;
            CHECK_EQ(al_merkle_proof(flat.data(), n, i, hashes, dirs, AL_MERKLE_MAX_STEPS, &steps), AL_OK);

            // Same proof as the C++ class, step by step.
            const MerkleProof p = MerkleTree(l).get_proof(i);
            CHECK_EQ(steps, static_cast<uint32_t>(p.steps.size()));
            for (uint32_t s = 0; s < steps && s < p.steps.size(); ++s) {
                CHECK(equals(hashes + 32 * s, p.steps[s].hash));
                CHECK_EQ(dirs[s], static_cast<uint8_t>(p.steps[s].direction));
            }

            CHECK_EQ(al_merkle_verify(root.data(), l[i].data(), hashes, dirs, steps), 1);
            const Hash256& other = l[(i + 1) % n];
            if (n > 1) CHECK_EQ(al_merkle_verify(root.data(), other.data(), hashes, dirs, steps), 0);
            if (steps > 0) {
                dirs[0] = 7; // invalid direction byte
                CHECK_EQ(al_merkle_verify(root.data(), l[i].data(), hashes, dirs, steps), 0);
            }
        }
    }
}

void test_merkle_proof_errors() {
    const std::vector<Hash256> l = leaves(5); // proofs have 3 steps
    const std::vector<uint8_t> flat = flatten(l);
    uint8_t hashes[32 * 3];
    uint8_t dirs[3];
    uint32_t steps = 0;

    CHECK_EQ(al_merkle_proof(flat.data(), 5, 5, hashes, dirs, 3, &steps), AL_ERR_ARG); // index == n
    CHECK_EQ(al_merkle_proof(flat.data(), 0, 0, hashes, dirs, 3, &steps), AL_ERR_ARG); // empty tree
    CHECK_EQ(al_merkle_proof(nullptr, 5, 0, hashes, dirs, 3, &steps), AL_ERR_ARG);
    CHECK_EQ(al_merkle_proof(flat.data(), 5, 0, hashes, dirs, 3, nullptr), AL_ERR_ARG);
    CHECK_EQ(al_merkle_proof(flat.data(), 5, 0, nullptr, dirs, 3, &steps), AL_ERR_ARG);

    // Too-small buffer: reported, with the required size filled in.
    steps = 0;
    CHECK_EQ(al_merkle_proof(flat.data(), 5, 0, hashes, dirs, 2, &steps), AL_ERR_BUFFER);
    CHECK_EQ(steps, 3u);
    steps = 0;
    CHECK_EQ(al_merkle_proof(flat.data(), 5, 0, nullptr, nullptr, 0, &steps), AL_ERR_BUFFER);
    CHECK_EQ(steps, 3u); // a size query with no buffers
}

void test_merkle_verify_errors() {
    const Hash256 h = leaves(1)[0];
    uint8_t hashes[32] = {0};
    uint8_t dirs[1] = {0};
    CHECK_EQ(al_merkle_verify(nullptr, h.data(), hashes, dirs, 1), AL_ERR_ARG);
    CHECK_EQ(al_merkle_verify(h.data(), nullptr, hashes, dirs, 1), AL_ERR_ARG);
    CHECK_EQ(al_merkle_verify(h.data(), h.data(), nullptr, dirs, 1), AL_ERR_ARG);
    CHECK_EQ(al_merkle_verify(h.data(), h.data(), hashes, nullptr, 1), AL_ERR_ARG);
    CHECK_EQ(al_merkle_verify(h.data(), h.data(), nullptr, nullptr, 0), 1); // one-leaf tree: leaf == root
    CHECK_EQ(al_merkle_verify(h.data(), h.data(), hashes, dirs, AL_MERKLE_MAX_STEPS + 1), 0);
}

} // namespace

int main() {
    test_sha256();
    test_merkle_root();
    test_merkle_proof_and_verify();
    test_merkle_proof_errors();
    test_merkle_verify_errors();
    return aletheia_test::finish("test_crypto_c");
}
