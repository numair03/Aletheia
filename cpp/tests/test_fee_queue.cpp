// Tests for FeePriorityQueue (cpp/include/aletheia/fee_queue.h): priority
// order (fee descending, then txid ascending), empty-queue errors, duplicates,
// removal by txid with Floyd heap construction, and a randomized comparison
// against a sorted-array model.

#include "aletheia/fee_queue.h"

#include "check.h"

#include <algorithm>
#include <cstdint>
#include <random>
#include <stdexcept>
#include <vector>

using namespace aletheia;

namespace {

Transaction make_tx(std::uint8_t from, std::uint64_t fee, std::uint64_t seq,
                    std::uint64_t amount = 1) {
    Transaction t;
    t.sender[0] = 0xAC;
    t.sender[31] = from;
    t.recipient[0] = 0xBD;
    t.amount = amount;
    t.fee = fee;
    t.seq = seq;
    return t;
}

// Pops everything; checks each popped txid matches its transaction.
std::vector<FeeEntry> drain(FeePriorityQueue& q) {
    std::vector<FeeEntry> out;
    while (!q.empty()) {
        out.push_back(q.pop());
        CHECK(out.back().txid == tx_id(out.back().tx));
    }
    return out;
}

bool sorted_by_priority(const std::vector<FeeEntry>& v) {
    for (std::size_t i = 1; i < v.size(); ++i) {
        if (FeePriorityQueue::higher(v[i], v[i - 1])) {
            return false;
        }
    }
    return true;
}

void test_empty() {
    FeePriorityQueue q;
    CHECK_EQ(q.size(), 0u);
    CHECK(q.empty());
    CHECK_THROWS(q.top(), std::out_of_range);
    CHECK_THROWS(q.pop(), std::out_of_range);
    CHECK_EQ(q.remove_txids({tx_id(make_tx(1, 1, 0))}), 0u);
    CHECK_EQ(q.remove_txids({}), 0u);
    CHECK(q.empty());
}

void test_fee_order() {
    FeePriorityQueue q;
    const std::uint64_t fees[] = {5, 1, 9, 3, 7, 0, UINT64_MAX, 2};
    std::uint64_t seq = 0;
    for (std::uint64_t f : fees) {
        q.push(make_tx(1, f, seq++));
    }
    CHECK_EQ(q.size(), 8u);
    CHECK_EQ(q.top().tx.fee, UINT64_MAX);
    CHECK_EQ(q.size(), 8u); // top does not remove

    const std::vector<FeeEntry> out = drain(q);
    const std::uint64_t want[] = {UINT64_MAX, 9, 7, 5, 3, 2, 1, 0};
    CHECK_EQ(out.size(), 8u);
    for (std::size_t i = 0; i < out.size() && i < 8; ++i) {
        CHECK_EQ(out[i].tx.fee, want[i]);
    }
    CHECK(q.empty());
    CHECK_THROWS(q.pop(), std::out_of_range);
}

void test_tie_break_by_txid() {
    // Equal fees: the smaller txid leaves first, whatever the push order.
    std::vector<Transaction> txs;
    for (std::uint8_t i = 0; i < 6; ++i) {
        txs.push_back(make_tx(i, 10, 0));
    }
    std::vector<Hash256> ids;
    for (const Transaction& t : txs) {
        ids.push_back(tx_id(t));
    }
    std::vector<Hash256> want = ids;
    std::sort(want.begin(), want.end());

    for (int order = 0; order < 2; ++order) {
        FeePriorityQueue q;
        if (order == 0) {
            for (const Transaction& t : txs) q.push(t);
        } else {
            for (std::size_t i = txs.size(); i-- > 0;) q.push(txs[i]);
        }
        const std::vector<FeeEntry> out = drain(q);
        CHECK_EQ(out.size(), want.size());
        for (std::size_t i = 0; i < out.size() && i < want.size(); ++i) {
            CHECK(out[i].txid == want[i]);
        }
    }

    // Fee still dominates the txid.
    FeePriorityQueue q;
    const Transaction low = make_tx(0, 10, 0);
    const Transaction high = make_tx(1, 11, 0);
    q.push(low);
    q.push(high);
    CHECK(q.pop().tx == high);
    CHECK(q.pop().tx == low);
}

void test_entry_contents() {
    FeePriorityQueue q;
    const Transaction t = make_tx(7, 42, 3, 1234);
    q.push(t);
    CHECK(q.top().tx == t);
    CHECK(q.top().txid == tx_id(t));
    const FeeEntry e = q.pop();
    CHECK(e.tx == t);
    CHECK(e.txid == tx_id(t));
    CHECK(q.empty());
}

void test_duplicates() {
    FeePriorityQueue q;
    const Transaction t = make_tx(1, 5, 0);
    q.push(t);
    q.push(t);
    q.push(make_tx(2, 5, 0));
    CHECK_EQ(q.size(), 3u);
    // Both copies go, and a repeated txid in the list is harmless.
    CHECK_EQ(q.remove_txids({tx_id(t), tx_id(t)}), 2u);
    CHECK_EQ(q.size(), 1u);
    CHECK(q.top().tx == make_tx(2, 5, 0));
}

void test_remove() {
    FeePriorityQueue q;
    std::vector<Transaction> txs;
    for (std::uint64_t i = 0; i < 50; ++i) {
        txs.push_back(make_tx(static_cast<std::uint8_t>(i), (i * 37) % 23, i));
        q.push(txs.back());
    }
    // Absent txids remove nothing.
    CHECK_EQ(q.remove_txids({tx_id(make_tx(200, 1, 0))}), 0u);
    CHECK_EQ(q.size(), 50u);

    // Remove the current top: the next one takes its place.
    const FeeEntry first = q.top();
    CHECK_EQ(q.remove_txids({first.txid}), 1u);
    CHECK_EQ(q.size(), 49u);
    CHECK(q.top().txid != first.txid);
    CHECK(!FeePriorityQueue::higher(q.top(), first));

    // Remove every third transaction plus an absent one.
    std::vector<Hash256> drop{tx_id(make_tx(201, 1, 0))};
    std::size_t expect_removed = 0;
    for (std::size_t i = 0; i < txs.size(); i += 3) {
        drop.push_back(tx_id(txs[i]));
        if (tx_id(txs[i]) != first.txid) ++expect_removed;
    }
    CHECK_EQ(q.remove_txids(drop), expect_removed);
    CHECK_EQ(q.size(), 49u - expect_removed);

    const std::vector<FeeEntry> out = drain(q);
    CHECK(sorted_by_priority(out));
    for (const FeeEntry& e : out) {
        CHECK(std::find(drop.begin(), drop.end(), e.txid) == drop.end());
        CHECK(e.txid != first.txid);
    }

    // Remove everything.
    FeePriorityQueue all;
    std::vector<Hash256> ids;
    for (const Transaction& t : txs) {
        all.push(t);
        ids.push_back(tx_id(t));
    }
    CHECK_EQ(all.remove_txids(ids), 50u);
    CHECK(all.empty());
    CHECK_THROWS(all.top(), std::out_of_range);
}

void test_remove_rebuilds_heap() {
    // For every size 1..40, removing every second pushed entry leaves a
    // compacted array that is generally not a heap; Floyd's construction must
    // restore the order (checked by draining).
    for (std::size_t n = 1; n <= 40; ++n) {
        FeePriorityQueue q;
        std::vector<Transaction> txs;
        for (std::size_t i = 0; i < n; ++i) {
            txs.push_back(make_tx(static_cast<std::uint8_t>(i), i % 7, i));
            q.push(txs.back());
        }
        std::vector<Hash256> drop;
        for (std::size_t i = 0; i < n; i += 2) {
            drop.push_back(tx_id(txs[i]));
        }
        CHECK_EQ(q.remove_txids(drop), drop.size());
        const std::vector<FeeEntry> out = drain(q);
        CHECK_EQ(out.size(), n - drop.size());
        CHECK(sorted_by_priority(out));
    }
}

void test_large() {
    FeePriorityQueue q;
    std::mt19937_64 rng(7);
    for (std::uint64_t i = 0; i < 10000; ++i) {
        q.push(make_tx(static_cast<std::uint8_t>(i), rng() % 1000, i));
    }
    const std::vector<FeeEntry> out = drain(q);
    CHECK_EQ(out.size(), 10000u);
    CHECK(sorted_by_priority(out));
}

// Random push/pop/top/remove against a plain array kept in priority order.
void test_random_model() {
    std::mt19937_64 rng(2026);
    FeePriorityQueue q;
    std::vector<FeeEntry> model; // sorted, highest priority first
    std::uint64_t seq = 0;
    bool ok = true;

    for (int step = 0; step < 20000; ++step) {
        const unsigned op = static_cast<unsigned>(rng() % 10);
        if (op < 5) {
            // Small fee range forces many ties; occasional exact duplicates.
            Transaction t = make_tx(static_cast<std::uint8_t>(rng() % 8), rng() % 6,
                                    (rng() % 4 == 0 && seq > 0) ? seq - 1 : seq);
            ++seq;
            q.push(t);
            const FeeEntry e{t, tx_id(t)};
            model.insert(std::upper_bound(model.begin(), model.end(), e,
                                          FeePriorityQueue::higher),
                         e);
        } else if (op < 8) {
            if (model.empty()) {
                ok = ok && q.empty();
                continue;
            }
            ok = ok && q.top().txid == model.front().txid;
            const FeeEntry e = q.pop();
            ok = ok && e.txid == model.front().txid && e.tx == model.front().tx;
            model.erase(model.begin());
        } else {
            // Remove a few random present txids and one absent one.
            std::vector<Hash256> drop{tx_id(make_tx(255, 0, ~0ULL))};
            for (int k = 0; k < 3 && !model.empty(); ++k) {
                drop.push_back(model[rng() % model.size()].txid);
            }
            const std::size_t before = model.size();
            model.erase(std::remove_if(model.begin(), model.end(),
                                       [&](const FeeEntry& e) {
                                           return std::find(drop.begin(), drop.end(),
                                                            e.txid) != drop.end();
                                       }),
                        model.end());
            ok = ok && q.remove_txids(drop) == before - model.size();
        }
        ok = ok && q.size() == model.size();
        if (!ok) break;
    }
    CHECK(ok);

    const std::vector<FeeEntry> out = drain(q);
    CHECK_EQ(out.size(), model.size());
    bool same = out.size() == model.size();
    for (std::size_t i = 0; same && i < out.size(); ++i) {
        same = out[i].txid == model[i].txid;
    }
    CHECK(same);
}

} // namespace

int main() {
    test_empty();
    test_fee_order();
    test_tie_break_by_txid();
    test_entry_contents();
    test_duplicates();
    test_remove();
    test_remove_rebuilds_heap();
    test_large();
    test_random_model();
    return aletheia_test::finish("test_fee_queue");
}
