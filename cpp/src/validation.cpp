#include "aletheia/validation.h"

#include "aletheia/merkle.h"

#include <limits>

namespace aletheia {

const char* to_string(RuleError e) {
    switch (e) {
    case RuleError::None: return "ok";
    case RuleError::Linkage: return "prev does not match the tip hash";
    case RuleError::Merkle: return "merkle root does not match the transactions";
    case RuleError::TxSet: return "empty block, duplicate txid, or too many transactions";
    case RuleError::Balance: return "insufficient balance";
    case RuleError::Seq: return "wrong sequence number";
    case RuleError::Overflow: return "amount overflow";
    }
    return "unknown";
}

Account StateOverlay::get(const Hash256& id) const {
    if (const Account* a = changes_.find(id)) return *a;
    if (const Account* a = base_.find(id)) return *a;
    return Account{};
}

void StateOverlay::commit_to(WorldState& target) const {
    changes_.for_each([&](const Hash256& id, const Account& a) { target.insert_or_assign(id, a); });
}

RuleError apply_tx(StateOverlay& state, const Transaction& tx) {
    constexpr std::uint64_t MAX = std::numeric_limits<std::uint64_t>::max();
    if (tx.amount > MAX - tx.fee) return RuleError::Overflow;
    const std::uint64_t cost = tx.amount + tx.fee;

    Account sender = state.get(tx.sender);
    if (tx.seq != sender.next_seq) return RuleError::Seq;
    if (sender.balance < cost) return RuleError::Balance;

    sender.balance -= cost;
    sender.next_seq += 1;

    // For a transfer to oneself the recipient is the already-debited sender.
    const bool self = tx.recipient == tx.sender;
    Account recipient = self ? sender : state.get(tx.recipient);
    if (recipient.balance > MAX - tx.amount) return RuleError::Overflow; // nothing written yet
    recipient.balance += tx.amount;

    if (!self) state.set(tx.sender, sender);
    state.set(tx.recipient, recipient);
    return RuleError::None;
}

RuleError LinkageRule::check(BlockContext& ctx) const {
    return ctx.block.header.prev == ctx.tip_hash ? RuleError::None : RuleError::Linkage;
}

RuleError MerkleRootRule::check(BlockContext& ctx) const {
    return MerkleTree(ctx.txids).root() == ctx.block.header.merkle_root ? RuleError::None
                                                                        : RuleError::Merkle;
}

RuleError TransactionSetRule::check(BlockContext& ctx) const {
    if (ctx.txids.empty()) return RuleError::TxSet;
    // Transaction positions are stored as uint32 (TxLocation::index).
    if (ctx.txids.size() > std::numeric_limits<std::uint32_t>::max()) return RuleError::TxSet;
    HashMap<Hash256, bool, Hash256Hasher> seen(ctx.txids.size() * 2);
    for (const Hash256& id : ctx.txids) {
        if (!seen.insert_or_assign(id, true)) return RuleError::TxSet;
    }
    return RuleError::None;
}

RuleError StateTransitionRule::check(BlockContext& ctx) const {
    for (const Transaction& tx : ctx.block.txs) {
        const RuleError e = apply_tx(ctx.state, tx);
        if (e != RuleError::None) return e;
    }
    return RuleError::None;
}

std::vector<std::unique_ptr<ValidationRule>> default_rules() {
    std::vector<std::unique_ptr<ValidationRule>> rules;
    rules.push_back(std::make_unique<LinkageRule>());
    rules.push_back(std::make_unique<MerkleRootRule>());
    rules.push_back(std::make_unique<TransactionSetRule>());
    rules.push_back(std::make_unique<StateTransitionRule>());
    return rules;
}

} // namespace aletheia
