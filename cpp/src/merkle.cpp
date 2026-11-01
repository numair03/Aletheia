#include "aletheia/merkle.h"
#include "aletheia/sha256.h"

#include <stdexcept>
#include <utility>

namespace aletheia {

MerkleTree::MerkleTree()
    : root_(ZERO_HASH) {}

MerkleTree::MerkleTree(const std::vector<Hash256>& leaves)
    : leaves_(leaves), root_(ZERO_HASH) {
    build_tree();
}

Hash256 MerkleTree::root() const {
    return root_;
}

size_t MerkleTree::leaf_count() const {
    return leaves_.size();
}

void MerkleTree::build_tree() {
    levels_.clear();
    if (leaves_.empty()) {
        root_ = ZERO_HASH;
        return;
    }

    levels_.push_back(leaves_);

    while (levels_.back().size() > 1) {
        const std::vector<Hash256>& current_level = levels_.back();
        std::vector<Hash256> next_level;
        size_t n = current_level.size();

        for (size_t i = 0; i < n; i += 2) {
            const Hash256& left = current_level[i];
            // If odd leaf count at this level, duplicate the last leaf
            const Hash256& right = (i + 1 < n) ? current_level[i + 1] : left;
            next_level.push_back(SHA256::hash(left, right));
        }

        levels_.push_back(std::move(next_level));
    }

    root_ = levels_.back()[0];
}

MerkleProof MerkleTree::get_proof(size_t index) const {
    if (index >= leaves_.size()) {
        throw std::out_of_range("MerkleTree: leaf index out of bounds");
    }

    MerkleProof proof;
    proof.leaf_index = index;

    size_t curr_idx = index;
    // Walk from leaf level up to the level below the root
    for (size_t level_idx = 0; level_idx + 1 < levels_.size(); ++level_idx) {
        const std::vector<Hash256>& level = levels_[level_idx];
        size_t level_size = level.size();

        if (curr_idx % 2 == 0) {
            // Current node is on the left; sibling is on the right
            Hash256 sibling = (curr_idx + 1 < level_size) ? level[curr_idx + 1] : level[curr_idx];
            proof.steps.push_back({sibling, ProofDirection::Right});
        } else {
            // Current node is on the right; sibling is on the left
            Hash256 sibling = level[curr_idx - 1];
            proof.steps.push_back({sibling, ProofDirection::Left});
        }

        curr_idx /= 2;
    }

    return proof;
}

bool MerkleTree::verify_proof(const Hash256& root, const Hash256& leaf, const MerkleProof& proof) {
    Hash256 current = leaf;

    for (const auto& step : proof.steps) {
        if (step.direction == ProofDirection::Left) {
            // Sibling is on the left: hash(sibling, current)
            current = SHA256::hash(step.hash, current);
        } else if (step.direction == ProofDirection::Right) {
            // Sibling is on the right: hash(current, sibling)
            current = SHA256::hash(current, step.hash);
        } else {
            // Not a valid ProofDirection value (reachable only via an out-of-range cast).
            return false;
        }
    }

    return current == root;
}

} // namespace aletheia
