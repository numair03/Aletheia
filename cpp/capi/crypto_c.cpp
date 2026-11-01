#include "crypto_c.h"

#include "aletheia/merkle.h"
#include "aletheia/sha256.h"

#include <cstring>
#include <limits>
#include <vector>

using aletheia::Hash256;
using aletheia::MerkleProof;
using aletheia::MerkleTree;
using aletheia::ProofDirection;
using aletheia::SHA256;
using aletheia::capi::guarded;

namespace {

Hash256 load_hash(const uint8_t* p) {
    Hash256 h;
    std::memcpy(h.data(), p, h.size());
    return h;
}

// Copies n contiguous 32-byte hashes into a vector. Returns false if n is too
// large to address, so a hostile n cannot overflow the size computation.
bool load_leaves(const uint8_t* leaves, uint64_t n, std::vector<Hash256>& out) {
    if (n > std::numeric_limits<size_t>::max() / 32) return false;
    out.resize(static_cast<size_t>(n));
    for (size_t i = 0; i < out.size(); ++i) out[i] = load_hash(leaves + 32 * i);
    return true;
}

} // namespace

extern "C" {

int32_t al_sha256(const uint8_t* data, size_t len, uint8_t out[32]) {
    return guarded([&]() -> int32_t {
        if (out == nullptr || (data == nullptr && len != 0)) return AL_ERR_ARG;
        const Hash256 h = SHA256::hash(data, len);
        std::memcpy(out, h.data(), h.size());
        return AL_OK;
    });
}

int32_t al_merkle_root(const uint8_t* leaves, uint64_t n, uint8_t out[32]) {
    return guarded([&]() -> int32_t {
        if (out == nullptr || (leaves == nullptr && n != 0)) return AL_ERR_ARG;
        std::vector<Hash256> v;
        if (!load_leaves(leaves, n, v)) return AL_ERR_ARG;
        const Hash256 root = MerkleTree(v).root();
        std::memcpy(out, root.data(), root.size());
        return AL_OK;
    });
}

int32_t al_merkle_proof(const uint8_t* leaves, uint64_t n, uint64_t index,
                        uint8_t* out_hashes, uint8_t* out_dirs, uint32_t cap,
                        uint32_t* out_steps) {
    return guarded([&]() -> int32_t {
        if (leaves == nullptr || out_steps == nullptr || index >= n) return AL_ERR_ARG;
        if (cap != 0 && (out_hashes == nullptr || out_dirs == nullptr)) return AL_ERR_ARG;
        std::vector<Hash256> v;
        if (!load_leaves(leaves, n, v)) return AL_ERR_ARG;
        const MerkleProof proof = MerkleTree(v).get_proof(static_cast<size_t>(index));
        const uint32_t steps = static_cast<uint32_t>(proof.steps.size());
        *out_steps = steps;
        if (steps > cap) return AL_ERR_BUFFER;
        for (uint32_t i = 0; i < steps; ++i) {
            std::memcpy(out_hashes + 32 * i, proof.steps[i].hash.data(), 32);
            out_dirs[i] = static_cast<uint8_t>(proof.steps[i].direction);
        }
        return AL_OK;
    });
}

int32_t al_merkle_verify(const uint8_t root[32], const uint8_t leaf[32],
                         const uint8_t* hashes, const uint8_t* dirs, uint32_t steps) {
    return guarded([&]() -> int32_t {
        if (root == nullptr || leaf == nullptr) return AL_ERR_ARG;
        if (steps != 0 && (hashes == nullptr || dirs == nullptr)) return AL_ERR_ARG;
        if (steps > AL_MERKLE_MAX_STEPS) return 0; // no real tree is this deep
        MerkleProof proof;
        proof.leaf_index = 0; // not used by verification
        proof.steps.resize(steps);
        for (uint32_t i = 0; i < steps; ++i) {
            if (dirs[i] > 1) return 0; // neither Left nor Right: invalid proof
            proof.steps[i].hash = load_hash(hashes + 32 * i);
            proof.steps[i].direction = static_cast<ProofDirection>(dirs[i]);
        }
        return MerkleTree::verify_proof(load_hash(root), load_hash(leaf), proof) ? 1 : 0;
    });
}

} // extern "C"
