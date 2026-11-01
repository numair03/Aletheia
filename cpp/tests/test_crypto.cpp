// Tests for SHA-256 (sha256.h), Merkle trees (merkle.h) and hex helpers (types.h).
//
// Every expected digest below was computed independently of this code base:
// the NIST FIPS 180-4 example vectors, and Python's hashlib for the rest.

#include "aletheia/merkle.h"
#include "aletheia/sha256.h"
#include "aletheia/types.h"
#include "check.h"

#include <cstring>
#include <stdexcept>
#include <string>
#include <vector>

using namespace aletheia;

namespace {

std::string sha_hex(const std::string& s) {
    return to_hex(SHA256::hash(reinterpret_cast<const uint8_t*>(s.data()), s.size()));
}

// Bytes 0, 1, 2, ... wrapping at 251, so every padding case uses non-trivial data.
std::vector<uint8_t> pattern(size_t n) {
    std::vector<uint8_t> v(n);
    for (size_t i = 0; i < n; ++i) v[i] = static_cast<uint8_t>(i % 251);
    return v;
}

// Leaf i = SHA-256 of the single byte i (same construction as the Python reference).
std::vector<Hash256> leaves(size_t n) {
    std::vector<Hash256> out;
    for (size_t i = 0; i < n; ++i) {
        const uint8_t b = static_cast<uint8_t>(i);
        out.push_back(SHA256::hash(&b, 1));
    }
    return out;
}

void test_sha256_nist_vectors() {
    CHECK_EQ(sha_hex(""), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
    CHECK_EQ(sha_hex("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
    CHECK_EQ(sha_hex("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq"),
             "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1");
    CHECK_EQ(sha_hex("abcdefghbcdefghicdefghijdefghijkefghijklfghijklmghijklmn"
                     "hijklmnoijklmnopjklmnopqklmnopqrlmnopqrsmnopqrstnopqrstu"),
             "cf5b16a778af8380036ce59e7b0492370b249b11e8f07a51afac45037afee9d1");
    CHECK_EQ(sha_hex(std::string(1000000, 'a')),
             "cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0");
}

// Lengths around the 64-byte block boundary exercise every padding branch:
// 55 bytes still fits the length field in one block, 56 does not, and so on.
void test_sha256_padding_boundaries() {
    const struct { size_t len; const char* hex; } cases[] = {
        {55, "463eb28e72f82e0a96c0a4cc53690c571281131f672aa229e0d45ae59b598b59"},
        {56, "da2ae4d6b36748f2a318f23e7ab1dfdf45acdc9d049bd80e59de82a60895f562"},
        {63, "29af2686fd53374a36b0846694cc342177e428d1647515f078784d69cdb9e488"},
        {64, "fdeab9acf3710362bd2658cdc9a29e8f9c757fcf9811603a8c447cd1d9151108"},
        {65, "4bfd2c8b6f1eec7a2afeb48b934ee4b2694182027e6d0fc075074f2fabb31781"},
        {119, "da18797ed7c3a777f0847f429724a2d8cd5138e6ed2895c3fa1a6d39d18f7ec6"},
        {120, "f52b23db1fbb6ded89ef42a23ce0c8922c45f25c50b568a93bf1c075420bbb7c"},
    };
    for (const auto& c : cases) {
        CHECK_EQ(to_hex(SHA256::hash(pattern(c.len))), c.hex);
    }
}

void test_sha256_streaming_and_reuse() {
    const std::vector<uint8_t> msg = pattern(1000);

    // Feeding the message in uneven pieces must equal one-shot hashing.
    SHA256 h;
    h.update(msg.data(), 3);
    h.update(msg.data() + 3, 500);
    h.update(nullptr, 0); // an empty update is a no-op
    h.update(msg.data() + 503, 497);
    const Hash256 streamed = h.finalize();
    CHECK(streamed == SHA256::hash(msg));

    // finalize() resets the object: hashing the same message again gives the
    // same digest, and a different message gives that message's digest.
    h.update(msg);
    CHECK(h.finalize() == streamed);
    CHECK(h.finalize() == SHA256::hash(nullptr, 0)); // nothing fed since the reset
    h.update(reinterpret_cast<const uint8_t*>("abc"), 3);
    CHECK_EQ(to_hex(h.finalize()), sha_hex("abc"));
}

void test_sha256_pair_hash() {
    // hash(left, right) is SHA-256 over the 64-byte concatenation, left first.
    const std::vector<Hash256> l = leaves(2);
    std::vector<uint8_t> concat(64);
    std::memcpy(concat.data(), l[0].data(), 32);
    std::memcpy(concat.data() + 32, l[1].data(), 32);
    CHECK(SHA256::hash(l[0], l[1]) == SHA256::hash(concat));
    CHECK(SHA256::hash(l[0], l[1]) != SHA256::hash(l[1], l[0]));
}

void test_hex_helpers() {
    const Hash256 h = SHA256::hash(pattern(10));
    const auto back = from_hex(to_hex(h));
    CHECK(back.has_value() && *back == h);

    const auto zero = from_hex(std::string(64, '0'));
    CHECK(zero.has_value() && *zero == ZERO_HASH); // a valid all-zero hash is not a failure

    CHECK(from_hex(std::string(64, 'A')).has_value()); // upper case accepted
    CHECK(!from_hex("").has_value());
    CHECK(!from_hex(std::string(63, '0')).has_value());
    CHECK(!from_hex(std::string(65, '0')).has_value());
    CHECK(!from_hex("0x" + std::string(62, '0')).has_value());
    CHECK(!from_hex(std::string(63, '0') + "g").has_value());
    CHECK(!from_hex(" " + std::string(63, '0')).has_value());
}

void test_merkle_known_roots() {
    // Roots from an independent Python implementation of the same rules:
    // parent = SHA-256(left || right); an odd level pairs its last node with itself.
    const struct { size_t n; const char* hex; } cases[] = {
        {1, "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d"},
        {2, "30e1867424e66e8b6d159246db94e3486778136f7e386ff5f001859d6b8484ab"},
        {3, "f2dcdd96791b6bac5d554f2d320e594b834f5da1981812c3707e7772234cb0ad"},
        {4, "9675e04b4ba9dc81b06e81731e2d21caa2c95557a85dcfa3fff70c9ff0f30b2e"},
        {5, "9674600fd139741c0f7dd7a32d984a0e74401cc90e6e8e5d203ed973d27324fe"},
        {7, "e263b77a6d80c1c56f3f67d1e0d803ad8eb2ac9d66c82f78735207c886a1592c"},
        {8, "0727b310f87099c1ba2ec0ba408def82c308237c8577f0bdfd2643e9cc6b7578"},
    };
    for (const auto& c : cases) {
        const MerkleTree t(leaves(c.n));
        CHECK_EQ(t.leaf_count(), c.n);
        CHECK_EQ(to_hex(t.root()), c.hex);
    }
    // A single leaf is its own root (M-R4).
    CHECK(MerkleTree(leaves(1)).root() == leaves(1)[0]);
}

void test_merkle_proofs_verify() {
    for (size_t n = 1; n <= 33; ++n) {
        const std::vector<Hash256> l = leaves(n);
        const MerkleTree t(l);
        // Proof length is ceil(log2 n): the number of levels below the root.
        size_t depth = 0;
        while ((size_t{1} << depth) < n) ++depth;
        for (size_t i = 0; i < n; ++i) {
            const MerkleProof p = t.get_proof(i);
            CHECK_EQ(p.leaf_index, i);
            CHECK_EQ(p.steps.size(), depth);
            CHECK(MerkleTree::verify_proof(t.root(), l[i], p));
        }
    }
}

void test_merkle_tampering_rejected() {
    const std::vector<Hash256> l = leaves(7);
    const MerkleTree t(l);
    for (size_t i = 0; i < l.size(); ++i) {
        const MerkleProof p = t.get_proof(i);

        Hash256 wrong_leaf = l[i];
        wrong_leaf[0] ^= 1;
        CHECK(!MerkleTree::verify_proof(t.root(), wrong_leaf, p));

        Hash256 wrong_root = t.root();
        wrong_root[31] ^= 1;
        CHECK(!MerkleTree::verify_proof(wrong_root, l[i], p));

        Hash256 cur = l[i]; // node value at the current level while walking up
        for (size_t s = 0; s < p.steps.size(); ++s) {
            MerkleProof bad_sibling = p;
            bad_sibling.steps[s].hash[5] ^= 1;
            CHECK(!MerkleTree::verify_proof(t.root(), l[i], bad_sibling));

            // Flipping a direction must fail, except when the sibling equals the
            // current node (the duplicated last node), where order cannot matter.
            MerkleProof flipped = p;
            flipped.steps[s].direction = p.steps[s].direction == ProofDirection::Left
                                             ? ProofDirection::Right
                                             : ProofDirection::Left;
            const bool self_paired = (p.steps[s].hash == cur);
            if (!self_paired) {
                CHECK(!MerkleTree::verify_proof(t.root(), l[i], flipped));
            }
            cur = p.steps[s].direction == ProofDirection::Left ? SHA256::hash(p.steps[s].hash, cur)
                                                               : SHA256::hash(cur, p.steps[s].hash);
        }
        CHECK(cur == t.root());

        // A proof for one leaf must not verify a different leaf.
        CHECK(!MerkleTree::verify_proof(t.root(), l[(i + 1) % l.size()], p));

        // A direction value other than Left/Right is rejected.
        MerkleProof bad_dir = p;
        bad_dir.steps[0].direction = static_cast<ProofDirection>(2);
        CHECK(!MerkleTree::verify_proof(t.root(), l[i], bad_dir));
    }
}

void test_merkle_edge_cases() {
    const MerkleTree empty;
    CHECK_EQ(empty.leaf_count(), size_t{0});
    CHECK(empty.root() == ZERO_HASH);
    CHECK(MerkleTree(std::vector<Hash256>{}).root() == ZERO_HASH);
    CHECK_THROWS(empty.get_proof(0), std::out_of_range);
    CHECK_THROWS(MerkleTree(leaves(4)).get_proof(4), std::out_of_range);

    // Documented properties that the ledger rules M-R5 and M-R6 build on:
    // M-R5: the empty tree and a one-leaf tree whose leaf is ZERO_HASH share a root.
    CHECK(MerkleTree(std::vector<Hash256>{ZERO_HASH}).root() == empty.root());
    // M-R6: duplicating the last leaf does not change the root.
    std::vector<Hash256> abc = leaves(3);
    std::vector<Hash256> abcc = abc;
    abcc.push_back(abc[2]);
    CHECK(MerkleTree(abc).root() == MerkleTree(abcc).root());
}

} // namespace

int main() {
    test_sha256_nist_vectors();
    test_sha256_padding_boundaries();
    test_sha256_streaming_and_reuse();
    test_sha256_pair_hash();
    test_hex_helpers();
    test_merkle_known_roots();
    test_merkle_proofs_verify();
    test_merkle_tampering_rejected();
    test_merkle_edge_cases();
    return aletheia_test::finish("test_crypto");
}
