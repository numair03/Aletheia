#include "aletheia/fee_queue.h"

#include "aletheia/hash_map.h"

#include <stdexcept>
#include <utility>

namespace aletheia {

bool FeePriorityQueue::higher(const FeeEntry& a, const FeeEntry& b) {
    if (a.tx.fee != b.tx.fee) {
        return a.tx.fee > b.tx.fee;
    }
    return a.txid < b.txid; // std::array compares lexicographically
}

void FeePriorityQueue::push(const Transaction& tx) {
    heap_.push_back(FeeEntry{tx, tx_id(tx)});
    sift_up(heap_.size() - 1);
}

const FeeEntry& FeePriorityQueue::top() const {
    if (heap_.empty()) {
        throw std::out_of_range("FeePriorityQueue::top on empty queue");
    }
    return heap_[0];
}

FeeEntry FeePriorityQueue::pop() {
    if (heap_.empty()) {
        throw std::out_of_range("FeePriorityQueue::pop on empty queue");
    }
    FeeEntry out = std::move(heap_[0]);
    // Move the last leaf to the root and let it sink to its place.
    heap_[0] = std::move(heap_.back());
    heap_.pop_back();
    if (!heap_.empty()) {
        sift_down(0);
    }
    return out;
}

std::size_t FeePriorityQueue::remove_txids(const std::vector<Hash256>& txids) {
    if (heap_.empty() || txids.empty()) {
        return 0;
    }
    // Set of txids to drop, sized so it never rebuilds while filling.
    HashMap<Hash256, bool, Hash256Hasher> drop(2 * txids.size());
    for (const Hash256& id : txids) {
        drop.insert_or_assign(id, true);
    }

    // Stable in-place compaction of the survivors.
    std::size_t kept = 0;
    for (std::size_t r = 0; r < heap_.size(); ++r) {
        if (!drop.contains(heap_[r].txid)) {
            if (kept != r) {
                heap_[kept] = std::move(heap_[r]);
            }
            ++kept;
        }
    }
    const std::size_t removed = heap_.size() - kept;
    heap_.resize(kept);

    // Floyd's construction: leaves (indices >= kept / 2) are already heaps;
    // sifting each internal node down, from the last one to the root, makes
    // every subtree a heap. Total work is O(n).
    if (removed != 0) {
        for (std::size_t i = kept / 2; i-- > 0;) {
            sift_down(i);
        }
    }
    return removed;
}

void FeePriorityQueue::sift_up(std::size_t i) {
    while (i > 0) {
        const std::size_t parent = (i - 1) / 2;
        if (!higher(heap_[i], heap_[parent])) {
            return;
        }
        std::swap(heap_[i], heap_[parent]);
        i = parent;
    }
}

void FeePriorityQueue::sift_down(std::size_t i) {
    const std::size_t n = heap_.size();
    for (;;) {
        const std::size_t left = 2 * i + 1;
        if (left >= n) {
            return;
        }
        // The child with priority; the right child exists only if right < n.
        std::size_t best = left;
        const std::size_t right = left + 1;
        if (right < n && higher(heap_[right], heap_[left])) {
            best = right;
        }
        if (!higher(heap_[best], heap_[i])) {
            return;
        }
        std::swap(heap_[i], heap_[best]);
        i = best;
    }
}

} // namespace aletheia
