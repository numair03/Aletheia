#include "aletheia/ledger.h"

#include <stdexcept>

namespace aletheia {

Ledger::Ledger(const BlockHeader& genesis, const std::vector<Allocation>& allocations)
    : rules_(default_rules()) {
    if (genesis.prev != ZERO_HASH) throw std::invalid_argument("genesis prev must be zero");
    if (genesis.merkle_root != ZERO_HASH) {
        throw std::invalid_argument("genesis has no transactions; merkle_root must be zero");
    }
    for (const Allocation& a : allocations) {
        if (!state_.insert_or_assign(a.account, Account{a.balance, 0})) {
            throw std::invalid_argument("duplicate genesis allocation");
        }
    }
    blocks_.push_back(StoredBlock{Block{genesis, {}}, header_hash(genesis)});
}

const Ledger::StoredBlock& Ledger::at(std::uint64_t height) const {
    if (height >= blocks_.size()) throw std::out_of_range("block height beyond tip");
    return blocks_[height];
}

const BlockHeader& Ledger::header_at(std::uint64_t height) const { return at(height).block.header; }
const Hash256& Ledger::hash_at(std::uint64_t height) const { return at(height).hash; }
const std::vector<Transaction>& Ledger::txs_at(std::uint64_t height) const {
    return at(height).block.txs;
}

Account Ledger::account(const Hash256& id) const {
    const Account* a = state_.find(id);
    return a ? *a : Account{};
}

std::optional<TxLocation> Ledger::find_tx(const Hash256& txid) const {
    const TxLocation* loc = tx_index_.find(txid);
    if (!loc) return std::nullopt;
    return *loc;
}

RuleError Ledger::check_tx(const Transaction& tx) const {
    StateOverlay scratch(state_);
    return apply_tx(scratch, tx);
}

RuleError Ledger::check_txs(const std::vector<Transaction>& txs, std::size_t* bad_index) const {
    StateOverlay scratch(state_);
    HashMap<Hash256, bool, Hash256Hasher> seen(txs.size() * 2);
    for (std::size_t i = 0; i < txs.size(); ++i) {
        RuleError e = seen.insert_or_assign(tx_id(txs[i]), true) ? apply_tx(scratch, txs[i])
                                                                 : RuleError::TxSet;
        if (e != RuleError::None) {
            if (bad_index) *bad_index = i;
            return e;
        }
    }
    return RuleError::None;
}

RuleError Ledger::append(const Block& block) {
    const std::vector<Hash256> ids = tx_ids(block.txs);
    StateOverlay pending(state_);
    BlockContext ctx{tip_hash(), block, ids, pending};
    for (const auto& rule : rules_) {
        const RuleError e = rule->check(ctx);
        if (e != RuleError::None) return e;
    }

    // Every rule passed: commit.
    const std::uint64_t height = blocks_.size();
    pending.commit_to(state_);
    for (std::uint32_t i = 0; i < ids.size(); ++i) tx_index_.insert_or_assign(ids[i], TxLocation{height, i});
    blocks_.push_back(StoredBlock{block, header_hash(block.header)});
    return RuleError::None;
}

} // namespace aletheia
