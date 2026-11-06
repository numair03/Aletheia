#pragma once

#include "aletheia/block.h"
#include "aletheia/types.h"

#include <cstddef>
#include <vector>

namespace aletheia {

// A pending transaction together with its txid, computed once on push.
struct FeeEntry {
    Transaction tx;
    Hash256 txid{};
};

// Fee-priority queue for the mempool: a binary max-heap written from scratch.
// std::vector is only the array; sift-up, sift-down and heap construction are
// implemented here.
//
// Priority: higher fee first; equal fees are broken by the smaller txid
// (lexicographic bytes), so the order is deterministic and identical on every
// node. Entries with the same txid are equal, and the queue is a multiset:
// pushing one transaction twice stores two copies (admission is the caller's
// job), and remove_txids removes every copy.
//
// Invariant (array layout, parent of i is (i - 1) / 2):
//   for every i > 0, heap_[i] does not have priority over heap_[(i - 1) / 2],
// so heap_[0] is always the highest-priority entry.
//
// Not thread-safe: the caller (D2's Go mempool) serialises every call.
class FeePriorityQueue {
public:
    // True if `a` must leave the queue before `b`.
    static bool higher(const FeeEntry& a, const FeeEntry& b);

    std::size_t size() const { return heap_.size(); }
    bool empty() const { return heap_.empty(); }

    // Inserts `tx` (its txid is computed here). O(log n).
    void push(const Transaction& tx);

    // The highest-priority entry. O(1). Throws std::out_of_range if empty.
    const FeeEntry& top() const;

    // Removes and returns the highest-priority entry. O(log n).
    // Throws std::out_of_range if empty.
    FeeEntry pop();

    // Removes every entry whose txid is in `txids` (repeats in `txids` are
    // harmless) and returns how many entries were removed. The survivors are
    // compacted in place and the heap is rebuilt bottom-up (Floyd), so the
    // cost is O(n + k) expected for n entries and k txids, rather than
    // O(k log n) plus a search per txid.
    std::size_t remove_txids(const std::vector<Hash256>& txids);

private:
    void sift_up(std::size_t i);
    void sift_down(std::size_t i);

    std::vector<FeeEntry> heap_;
};

} // namespace aletheia
