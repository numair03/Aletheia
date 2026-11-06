#pragma once

#include "aletheia/block.h"
#include "aletheia/hash_map.h"
#include "aletheia/validation.h"

#include <cstdint>
#include <memory>
#include <optional>
#include <vector>

namespace aletheia {

// Where a confirmed transaction lives: block height and position in the block.
struct TxLocation {
    std::uint64_t height = 0;
    std::uint32_t index = 0;
};

// A genesis balance.
struct Allocation {
    Hash256 account{};
    std::uint64_t balance = 0;
};

// The hash-linked chain of blocks plus the world state it produces.
//
// Structure: blocks are stored in height order (std::vector is only the
// storage); each header's `prev` is the hash of the header before it, and
// LinkageRule enforces that link on every append, so the sequence is a
// hash-linked list whose integrity anyone can re-check from the headers alone.
// Two hash maps index it: account -> Account (world state) and
// txid -> TxLocation (for Merkle proofs).
//
// append() is atomic: every ValidationRule runs against a StateOverlay, and
// the block, its txids and its state changes are committed only if all pass.
// A rejected block leaves the ledger exactly as it was.
//
// Not thread-safe; the Go binding guards each ledger with a lock.
class Ledger {
public:
    // Genesis has no transactions, so its merkle_root must be all zero
    // (the empty-tree root, M-R5), and its prev must be all zero.
    // Throws std::invalid_argument for a malformed genesis or an account that
    // appears twice in `allocations`.
    Ledger(const BlockHeader& genesis, const std::vector<Allocation>& allocations);

    Ledger(const Ledger&) = delete;
    Ledger& operator=(const Ledger&) = delete;

    // Height of the tip; genesis is height 0, so there are height() + 1 blocks.
    std::uint64_t height() const { return blocks_.size() - 1; }
    const Hash256& tip_hash() const { return blocks_.back().hash; }

    // Throw std::out_of_range if height > height().
    const BlockHeader& header_at(std::uint64_t height) const;
    const Hash256& hash_at(std::uint64_t height) const;
    const std::vector<Transaction>& txs_at(std::uint64_t height) const;

    Account account(const Hash256& id) const;
    std::optional<TxLocation> find_tx(const Hash256& txid) const;

    // Would `tx` be valid as the next transaction of its sender against the
    // committed state? (Mempool admission; changes nothing.)
    RuleError check_tx(const Transaction& tx) const;

    // Would `txs`, applied in order on top of the committed state, be valid
    // as the transactions of the next block? Returns the first failing rule
    // (TxSet for a repeated txid, otherwise Balance/Seq/Overflow) and stores
    // its position in *bad_index (if not null). An empty list returns None.
    // Changes nothing. If this returns None for a non-empty list, a block
    // holding exactly these transactions, with prev = tip_hash() and the
    // matching Merkle root, is accepted by append(). Used for block assembly:
    // transactions admitted one by one with check_tx() can still conflict
    // with each other (same sender and seq, or the same transaction twice).
    RuleError check_txs(const std::vector<Transaction>& txs, std::size_t* bad_index) const;

    // Validates `block` as the new tip and appends it, or returns the first
    // rule it breaks and changes nothing.
    RuleError append(const Block& block);

private:
    struct StoredBlock {
        Block block;
        Hash256 hash;
    };
    const StoredBlock& at(std::uint64_t height) const;

    std::vector<StoredBlock> blocks_;
    WorldState state_;
    HashMap<Hash256, TxLocation, Hash256Hasher> tx_index_;
    std::vector<std::unique_ptr<ValidationRule>> rules_;
};

} // namespace aletheia
