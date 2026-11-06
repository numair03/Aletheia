#pragma once

#include "aletheia/block.h"
#include "aletheia/hash_map.h"
#include "aletheia/types.h"

#include <cstdint>
#include <memory>
#include <vector>

namespace aletheia {

// World state: one entry per account. An account that is absent behaves as
// {balance 0, next_seq 0}.
struct Account {
    std::uint64_t balance = 0;
    std::uint64_t next_seq = 0;
};
using WorldState = HashMap<Hash256, Account, Hash256Hasher>;

// Why a block or transaction was rejected. None means accepted.
enum class RuleError : int {
    None = 0,
    Linkage,  // prev does not match the hash of the current tip
    Merkle,   // merkle_root does not match the block's transactions
    TxSet,    // empty block (M-R5), duplicate txid (M-R6), or too many transactions
    Balance,  // sender cannot pay amount + fee
    Seq,      // seq is not the sender's next sequence number
    Overflow, // amount + fee, or the recipient's new balance, exceeds 2^64 - 1
};
const char* to_string(RuleError e);

// The state changes of one block (or one candidate transaction), layered over
// the committed world state without modifying it. Only touched accounts are
// copied, so validating a block costs O(transactions), not O(accounts), and a
// rejected block leaves the committed state untouched.
class StateOverlay {
public:
    explicit StateOverlay(const WorldState& base) : base_(base) {}
    Account get(const Hash256& id) const;
    void set(const Hash256& id, const Account& a) { changes_.insert_or_assign(id, a); }
    void commit_to(WorldState& target) const;

private:
    const WorldState& base_;
    WorldState changes_;
};

// Applies one transaction to `state`, or returns why it is invalid (and then
// changes nothing). Checks, in order: amount + fee overflow, sequence number,
// balance, recipient overflow. A transaction to oneself costs only the fee.
// Used both for whole blocks and for single-transaction admission checks.
RuleError apply_tx(StateOverlay& state, const Transaction& tx);

// Everything a rule may inspect when a block is proposed for the tip.
struct BlockContext {
    const Hash256& tip_hash;            // hash of the current tip header
    const Block& block;                 // the candidate block
    const std::vector<Hash256>& txids;  // tx_ids(block.txs), computed once
    StateOverlay& state;                // pending state; only StateTransitionRule writes it
};

// One independent condition a block must satisfy before it is appended.
// Proof of work and difficulty are consensus rules and are checked by the Go
// consensus package before the ledger sees a block; these rules protect the
// integrity of the ledger itself.
class ValidationRule {
public:
    virtual ~ValidationRule() = default;
    virtual const char* name() const = 0;
    virtual RuleError check(BlockContext& ctx) const = 0;
};

// header.prev == hash of the current tip: keeps the list hash-linked.
class LinkageRule : public ValidationRule {
public:
    const char* name() const override { return "linkage"; }
    RuleError check(BlockContext& ctx) const override;
};

// header.merkle_root == Merkle root of the block's txids (M-R1..M-R4).
class MerkleRootRule : public ValidationRule {
public:
    const char* name() const override { return "merkle-root"; }
    RuleError check(BlockContext& ctx) const override;
};

// At least one transaction (M-R5: only genesis is empty) and no txid twice
// (M-R6). Repeating a transaction from an earlier block is impossible
// separately: its seq has already been used, so StateTransitionRule rejects it.
class TransactionSetRule : public ValidationRule {
public:
    const char* name() const override { return "transaction-set"; }
    RuleError check(BlockContext& ctx) const override;
};

// Applies every transaction in order to the pending state (apply_tx).
class StateTransitionRule : public ValidationRule {
public:
    const char* name() const override { return "state-transition"; }
    RuleError check(BlockContext& ctx) const override;
};

// The four rules in the order the ledger runs them: cheap structural checks
// first, the state transition last.
std::vector<std::unique_ptr<ValidationRule>> default_rules();

} // namespace aletheia
