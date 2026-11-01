#pragma once

#include "aletheia/types.h"

#include <cstddef>
#include <vector>

namespace aletheia {

/**
 * @brief MerkleTree represents a cryptographic binary hash tree constructed
 *        from an ordered set of leaf hashes (e.g. transaction IDs).
 *
 * It provides O(log n) SPV inclusion proof generation and verification.
 * For odd leaf counts at any tree level, the last leaf/node is duplicated
 * (proposed baseline rule matching standard SPV ledger conventions).
 */
class MerkleTree {
public:
    // Construct an empty Merkle tree (root is ZERO_HASH)
    MerkleTree();

    // Construct a Merkle tree from a sequence of 32-byte leaf hashes
    explicit MerkleTree(const std::vector<Hash256>& leaves);

    // Return the 32-byte Merkle root hash
    Hash256 root() const;

    // Return the number of original leaves in the tree
    size_t leaf_count() const;

    // Generate an SPV inclusion proof for the leaf at the given index
    // Throws std::out_of_range if index >= leaf_count()
    MerkleProof get_proof(size_t index) const;

    /**
     * @brief Check that folding the proof's sibling steps over `leaf` reproduces
     *        `root`. Uses only its arguments (no tree, no server), so it can run
     *        in the verifier's own trust domain.
     *
     * Cost is linear in proof.steps.size(); for a proof produced by get_proof()
     * on an n-leaf tree that is O(log n).
     *
     * @param root Expected Merkle root
     * @param leaf 32-byte leaf hash being proven
     * @param proof Sibling hashes and directions to fold, in order
     * @return true if every step direction is Left or Right and the folded value
     *         equals root; false otherwise.
     *
     * Not checked here (interface questions still open): proof.leaf_index, the
     * number of steps relative to the tree size, and whether root denotes an
     * empty tree.
     */
    static bool verify_proof(const Hash256& root, const Hash256& leaf, const MerkleProof& proof);

private:
    void build_tree();

    std::vector<Hash256> leaves_;
    std::vector<std::vector<Hash256>> levels_; // levels_[0] is leaves, levels_.back()[0] is root
    Hash256 root_;
};

} // namespace aletheia
