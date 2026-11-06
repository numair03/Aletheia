// Tests for the validation rules and the ledger
// (cpp/include/aletheia/{validation,ledger}.h).

#include "aletheia/ledger.h"
#include "aletheia/merkle.h"

#include "check.h"

#include <cstdint>
#include <limits>
#include <random>
#include <stdexcept>
#include <string>
#include <vector>

using namespace aletheia;

namespace {

constexpr std::uint64_t MAX = std::numeric_limits<std::uint64_t>::max();

Hash256 acct(std::uint8_t n) {
    Hash256 h{};
    h[0] = 0xAC;
    h[31] = n;
    return h;
}

const Hash256 A = acct(1), B = acct(2), C = acct(3), NOBODY = acct(99);

BlockHeader genesis_header() {
    BlockHeader g;
    g.version = 1;
    g.timestamp = 1700000000;
    g.difficulty = 16;
    return g;
}

Ledger make_ledger() {
    return Ledger(genesis_header(), {{A, 1000}, {B, 500}, {C, MAX}});
}

Transaction tx(const Hash256& from, const Hash256& to, std::uint64_t amount, std::uint64_t fee,
               std::uint64_t seq) {
    Transaction t;
    t.sender = from;
    t.recipient = to;
    t.amount = amount;
    t.fee = fee;
    t.seq = seq;
    return t;
}

// A correctly linked block with the correct Merkle root for `txs`.
Block next_block(const Ledger& l, const std::vector<Transaction>& txs) {
    Block b;
    b.header.version = 1;
    b.header.prev = l.tip_hash();
    b.header.merkle_root = MerkleTree(tx_ids(txs)).root();
    b.header.timestamp = 1700000000 + static_cast<std::int64_t>(l.height()) + 1;
    b.header.difficulty = 16;
    b.txs = txs;
    return b;
}

void test_genesis() {
    Ledger l = make_ledger();
    CHECK_EQ(l.height(), 0u);
    CHECK(l.tip_hash() == header_hash(genesis_header()));
    CHECK(l.header_at(0) == genesis_header());
    CHECK(l.txs_at(0).empty());
    CHECK_EQ(l.account(A).balance, 1000u);
    CHECK_EQ(l.account(A).next_seq, 0u);
    CHECK_EQ(l.account(NOBODY).balance, 0u);
    CHECK_THROWS(l.header_at(1), std::out_of_range);
    CHECK_THROWS(l.hash_at(1), std::out_of_range);

    BlockHeader bad = genesis_header();
    bad.merkle_root[0] = 1;
    CHECK_THROWS(Ledger(bad, {}), std::invalid_argument);
    bad = genesis_header();
    bad.prev[0] = 1;
    CHECK_THROWS(Ledger(bad, {}), std::invalid_argument);
    CHECK_THROWS(Ledger(genesis_header(), {{A, 1}, {A, 2}}), std::invalid_argument);
}

void test_valid_chain() {
    Ledger l = make_ledger();
    const Transaction t1 = tx(A, B, 100, 5, 0);
    const Transaction t2 = tx(B, NOBODY, 50, 1, 0);
    CHECK(l.append(next_block(l, {t1, t2})) == RuleError::None);
    CHECK_EQ(l.height(), 1u);
    CHECK_EQ(l.account(A).balance, 895u);  // 1000 - 100 - 5
    CHECK_EQ(l.account(B).balance, 549u);  // 500 + 100 - 50 - 1
    CHECK_EQ(l.account(NOBODY).balance, 50u);
    CHECK_EQ(l.account(A).next_seq, 1u);
    CHECK_EQ(l.account(B).next_seq, 1u);
    CHECK_EQ(l.account(NOBODY).next_seq, 0u);

    // Same sender twice in one block, and spending funds received in the
    // same block (NOBODY now has 50).
    const Transaction t3 = tx(A, C, 0, 0, 1); // C is at MAX: amount 0 is fine
    const Transaction t4 = tx(A, NOBODY, 10, 0, 2);
    const Transaction t5 = tx(NOBODY, A, 60, 0, 0);
    CHECK(l.append(next_block(l, {t3, t4, t5})) == RuleError::None);
    CHECK_EQ(l.height(), 2u);
    CHECK_EQ(l.account(A).balance, 945u); // 895 - 10 + 60
    CHECK_EQ(l.account(NOBODY).balance, 0u);
    CHECK_EQ(l.account(A).next_seq, 3u);

    // Headers link, and lookups work.
    CHECK(l.header_at(2).prev == l.hash_at(1));
    CHECK(l.header_at(1).prev == l.hash_at(0));
    CHECK(l.tip_hash() == header_hash(l.header_at(2)));
    CHECK_EQ(l.txs_at(2).size(), 3u);
    CHECK(l.txs_at(1)[1] == t2);
    const auto loc = l.find_tx(tx_id(t4));
    CHECK(loc.has_value() && loc->height == 2 && loc->index == 1);
    const auto loc1 = l.find_tx(tx_id(t1));
    CHECK(loc1.has_value() && loc1->height == 1 && loc1->index == 0);
    CHECK(!l.find_tx(tx_id(tx(A, B, 1, 1, 99))).has_value());
}

void test_self_transfer_costs_fee() {
    Ledger l = make_ledger();
    CHECK(l.append(next_block(l, {tx(A, A, 400, 7, 0)})) == RuleError::None);
    CHECK_EQ(l.account(A).balance, 993u);
    CHECK_EQ(l.account(A).next_seq, 1u);
    // Exactly the whole balance may be moved to oneself.
    CHECK(l.append(next_block(l, {tx(A, A, 990, 3, 1)})) == RuleError::None);
    CHECK_EQ(l.account(A).balance, 990u);
}

// Each rule, broken on its own, rejects the block with its own error.
void test_each_rule_rejects() {
    Ledger l = make_ledger();
    const Transaction ok = tx(A, B, 1, 1, 0);

    Block b = next_block(l, {ok});
    b.header.prev[0] ^= 1;
    CHECK(l.append(b) == RuleError::Linkage);

    b = next_block(l, {ok});
    b.header.merkle_root[0] ^= 1;
    CHECK(l.append(b) == RuleError::Merkle);

    // Merkle root of the transactions in a different order.
    const Transaction ok2 = tx(B, A, 1, 1, 0);
    b = next_block(l, {ok, ok2});
    std::swap(b.txs[0], b.txs[1]);
    CHECK(l.append(b) == RuleError::Merkle);

    CHECK(l.append(next_block(l, {})) == RuleError::TxSet);      // M-R5
    CHECK(l.append(next_block(l, {ok, ok})) == RuleError::TxSet); // M-R6

    CHECK(l.append(next_block(l, {tx(A, B, 1000, 1, 0)})) == RuleError::Balance);
    CHECK(l.append(next_block(l, {tx(NOBODY, B, 1, 0, 0)})) == RuleError::Balance);
    CHECK(l.append(next_block(l, {tx(A, B, 1, 1, 1)})) == RuleError::Seq);
    CHECK(l.append(next_block(l, {tx(A, B, MAX, 1, 0)})) == RuleError::Overflow);
    CHECK(l.append(next_block(l, {tx(A, C, 1, 0, 0)})) == RuleError::Overflow); // C at MAX

    // None of the rejections changed anything.
    CHECK_EQ(l.height(), 0u);
    CHECK_EQ(l.account(A).balance, 1000u);
    CHECK_EQ(l.account(A).next_seq, 0u);

    // A transaction from an earlier block cannot be repeated: its seq is used.
    CHECK(l.append(next_block(l, {ok})) == RuleError::None);
    CHECK(l.append(next_block(l, {ok})) == RuleError::Seq);
}

// A block whose last transaction is invalid commits none of them.
void test_append_is_atomic() {
    Ledger l = make_ledger();
    const Hash256 tip = l.tip_hash();
    const Transaction good = tx(A, B, 100, 1, 0);
    const Transaction bad = tx(B, C, 700, 1, 0); // B has 600 after `good`
    CHECK(l.append(next_block(l, {good, bad})) == RuleError::Balance);
    CHECK_EQ(l.height(), 0u);
    CHECK(l.tip_hash() == tip);
    CHECK_EQ(l.account(A).balance, 1000u);
    CHECK_EQ(l.account(A).next_seq, 0u);
    CHECK_EQ(l.account(B).balance, 500u);
    CHECK(!l.find_tx(tx_id(good)).has_value());

    // B would have enough once `good` is applied, so order within the block matters.
    const Transaction fine = tx(B, NOBODY, 550, 1, 0);
    CHECK(l.append(next_block(l, {good, fine})) == RuleError::None);
    CHECK_EQ(l.account(NOBODY).balance, 550u);
}

void test_check_tx() {
    Ledger l = make_ledger();
    CHECK(l.check_tx(tx(A, B, 999, 1, 0)) == RuleError::None);
    CHECK(l.check_tx(tx(A, B, 1000, 1, 0)) == RuleError::Balance);
    CHECK(l.check_tx(tx(A, B, 1, 1, 1)) == RuleError::Seq);
    CHECK(l.check_tx(tx(A, B, 1, MAX, 0)) == RuleError::Overflow);
    CHECK_EQ(l.account(A).next_seq, 0u); // unchanged
    CHECK_EQ(l.account(A).balance, 1000u);
}

// check_txs: transactions that each pass check_tx can still conflict; the
// list check finds the first conflict without changing anything.
void test_check_txs() {
    Ledger l = make_ledger();
    std::size_t bad = 99;
    CHECK(l.check_txs({}, &bad) == RuleError::None);
    CHECK_EQ(bad, 99u); // untouched on success

    const Transaction t1 = tx(A, B, 600, 1, 0);
    const Transaction t2 = tx(A, NOBODY, 600, 1, 0); // same sender and seq as t1
    CHECK(l.check_tx(t1) == RuleError::None && l.check_tx(t2) == RuleError::None);
    CHECK(l.check_txs({t1, t2}, &bad) == RuleError::Seq);
    CHECK_EQ(bad, 1u);
    CHECK(l.check_txs({t1, t1}, &bad) == RuleError::TxSet); // the same transaction twice
    CHECK_EQ(bad, 1u);
    CHECK(l.check_txs({t1, tx(A, B, 400, 0, 1)}, &bad) == RuleError::Balance); // 1000-601 < 400
    CHECK_EQ(bad, 1u);
    CHECK(l.check_txs({tx(A, B, 1, 0, 1), t1}, &bad) == RuleError::Seq); // order matters
    CHECK_EQ(bad, 0u);
    CHECK(l.check_txs({t1, tx(B, C, 1, 0, 0)}, &bad) == RuleError::Overflow); // C is at MAX
    CHECK(l.check_txs({t1, tx(B, C, 0, 0, 0), tx(A, B, 300, 0, 1)}, nullptr) == RuleError::None);
    CHECK_EQ(l.account(A).next_seq, 0u); // nothing changed
    CHECK_EQ(l.height(), 0u);

    // A list that passes is accepted by append.
    CHECK(l.append(next_block(l, {t1, tx(B, C, 0, 0, 0), tx(A, B, 300, 0, 1)})) == RuleError::None);
}

void test_rule_hierarchy() {
    const auto rules = default_rules();
    CHECK_EQ(rules.size(), 4u);
    CHECK_EQ(std::string(rules[0]->name()), std::string("linkage"));
    CHECK_EQ(std::string(rules[1]->name()), std::string("merkle-root"));
    CHECK_EQ(std::string(rules[2]->name()), std::string("transaction-set"));
    CHECK_EQ(std::string(rules[3]->name()), std::string("state-transition"));
    CHECK(std::string(to_string(RuleError::Seq)) == "wrong sequence number");
}

// Random transfers among five accounts, checked against a plain array model.
// Also: replaying the accepted blocks into a fresh ledger reproduces the same
// tip and state (what chain selection does when it switches chains), and the
// total supply falls by exactly the fees burned.
void test_random_against_model() {
    const Hash256 ids[5] = {acct(10), acct(11), acct(12), acct(13), acct(14)};
    std::vector<Allocation> alloc;
    std::uint64_t bal[5], seq[5] = {0, 0, 0, 0, 0}, supply = 0, burned = 0;
    for (int i = 0; i < 5; ++i) {
        bal[i] = 1000 * (i + 1);
        supply += bal[i];
        alloc.push_back({ids[i], bal[i]});
    }
    Ledger l(genesis_header(), alloc);
    std::vector<Block> accepted;
    std::mt19937_64 rng(42);

    bool ok = true;
    for (int round = 0; round < 300; ++round) {
        // Build a block of 1..8 transactions; some are invalid on purpose.
        std::vector<Transaction> txs;
        std::uint64_t mbal[5], mseq[5];
        std::copy(bal, bal + 5, mbal);
        std::copy(seq, seq + 5, mseq);
        bool block_valid = true;
        std::uint64_t block_fees = 0;
        const int n = 1 + static_cast<int>(rng() % 8);
        for (int k = 0; k < n; ++k) {
            const int s = static_cast<int>(rng() % 5), r = static_cast<int>(rng() % 5);
            const std::uint64_t amount = rng() % 700, fee = rng() % 5;
            const bool wrong_seq = rng() % 20 == 0;
            Transaction t = tx(ids[s], ids[r], amount, fee, mseq[s] + (wrong_seq ? 1 : 0));
            txs.push_back(t);
            if (wrong_seq || mbal[s] < amount + fee) {
                block_valid = false;
                break;
            }
            mbal[s] -= amount + fee;
            mbal[r] += amount;
            mseq[s] += 1;
            block_fees += fee;
        }
        // check_txs predicts append exactly, and points at the first bad one.
        std::size_t bad = 0;
        const RuleError predicted = l.check_txs(txs, &bad);
        ok = ok && (predicted == RuleError::None) == block_valid;
        if (!block_valid) ok = ok && bad == txs.size() - 1;

        const Block b = next_block(l, txs);
        const RuleError e = l.append(b);
        ok = ok && (e == RuleError::None) == block_valid && e == predicted;
        if (block_valid) {
            std::copy(mbal, mbal + 5, bal);
            std::copy(mseq, mseq + 5, seq);
            burned += block_fees;
            accepted.push_back(b);
        }
        std::uint64_t total = 0;
        for (int i = 0; i < 5; ++i) {
            ok = ok && l.account(ids[i]).balance == bal[i] && l.account(ids[i]).next_seq == seq[i];
            total += bal[i];
        }
        ok = ok && total + burned == supply;
    }
    CHECK(ok);
    CHECK(accepted.size() > 50); // the test exercised plenty of valid blocks
    CHECK_EQ(l.height(), accepted.size());

    Ledger replay(genesis_header(), alloc);
    bool replay_ok = true;
    for (const Block& b : accepted) replay_ok = replay_ok && replay.append(b) == RuleError::None;
    CHECK(replay_ok);
    CHECK(replay.tip_hash() == l.tip_hash());
    for (int i = 0; i < 5; ++i) CHECK_EQ(replay.account(ids[i]).balance, bal[i]);
}

} // namespace

int main() {
    test_genesis();
    test_valid_chain();
    test_self_transfer_costs_fee();
    test_each_rule_rejects();
    test_append_is_atomic();
    test_check_tx();
    test_check_txs();
    test_rule_hierarchy();
    test_random_against_model();
    return aletheia_test::finish("test_ledger");
}
