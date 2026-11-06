#pragma once

#include "aletheia/types.h"

#include <cstddef>
#include <cstdint>
#include <stdexcept>
#include <vector>

namespace aletheia {

// 64-bit FNV-1a over all 32 bytes of a hash. Account IDs are arbitrary
// 32-byte values (not necessarily SHA-256 outputs), so every byte is mixed in
// rather than trusting any single word to be uniform.
struct Hash256Hasher {
    std::uint64_t operator()(const Hash256& h) const {
        std::uint64_t x = 1469598103934665603ULL; // FNV offset basis
        for (std::uint8_t b : h) {
            x ^= b;
            x *= 1099511628211ULL;                // FNV prime
        }
        return x;
    }
};

// Hash map with open addressing and linear probing, written from scratch.
// std::vector is only the slot array; probing, collision handling, deletion
// and resizing are implemented here.
//
// Invariants:
//   * capacity is a power of two, so `hash & (capacity - 1)` picks the home slot;
//   * (size + tombstones) / capacity <= 0.7, so at least 30% of slots are Empty
//     and every probe sequence terminates at an Empty slot;
//   * a key appears in at most one Full slot, and that slot is reachable from
//     the key's home slot without crossing an Empty slot.
//
// erase() leaves a Tombstone rather than an Empty slot: emptying it would cut
// the probe chain of any key stored after it. Tombstones are reused by later
// inserts and discarded when the table is rebuilt.
//
// Complexity: insert/find/erase are expected O(1) at bounded load factor;
// a rebuild is O(capacity) and happens rarely enough to be amortised O(1).
// Not thread-safe; callers synchronise (the Go side holds a mutex).
template <typename K, typename V, typename Hasher>
class HashMap {
public:
    explicit HashMap(std::size_t min_capacity = 16) {
        std::size_t cap = 8;
        while (cap < min_capacity) cap *= 2;
        slots_.resize(cap);
    }

    std::size_t size() const { return size_; }
    bool empty() const { return size_ == 0; }
    std::size_t capacity() const { return slots_.size(); }
    std::size_t tombstones() const { return tombstones_; }

    // Inserts key -> value, or overwrites the value if the key is present.
    // Returns true if the key was newly inserted.
    bool insert_or_assign(const K& key, const V& value) {
        if ((size_ + tombstones_ + 1) * 10 > capacity() * 7) grow();
        const std::size_t mask = capacity() - 1;
        std::size_t i = hasher_(key) & mask;
        std::size_t reuse = NONE; // first tombstone on the probe path
        while (slots_[i].state != State::Empty) {
            if (slots_[i].state == State::Full && slots_[i].key == key) {
                slots_[i].value = value;
                return false;
            }
            if (slots_[i].state == State::Tombstone && reuse == NONE) reuse = i;
            i = (i + 1) & mask;
        }
        // Key is absent (we reached Empty). Prefer the earlier tombstone so
        // probe chains stay short.
        if (reuse != NONE) {
            i = reuse;
            --tombstones_;
        }
        slots_[i].key = key;
        slots_[i].value = value;
        slots_[i].state = State::Full;
        ++size_;
        return true;
    }

    // Returns a pointer to the stored value, or nullptr if absent. The pointer
    // is invalidated by the next insert_or_assign (which may rebuild the table).
    V* find(const K& key) {
        const std::size_t i = locate(key);
        return i == NONE ? nullptr : &slots_[i].value;
    }
    const V* find(const K& key) const {
        const std::size_t i = locate(key);
        return i == NONE ? nullptr : &slots_[i].value;
    }
    bool contains(const K& key) const { return locate(key) != NONE; }

    // Removes the key. Returns true if it was present.
    bool erase(const K& key) {
        const std::size_t i = locate(key);
        if (i == NONE) return false;
        slots_[i].state = State::Tombstone;
        slots_[i].key = K{};   // drop the old contents
        slots_[i].value = V{};
        --size_;
        ++tombstones_;
        return true;
    }

    void clear() {
        for (Slot& s : slots_) s = Slot{};
        size_ = 0;
        tombstones_ = 0;
    }

    // Calls fn(key, value) for every entry, in slot order (unspecified order).
    template <typename Fn>
    void for_each(Fn&& fn) const {
        for (const Slot& s : slots_) {
            if (s.state == State::Full) fn(s.key, s.value);
        }
    }

private:
    enum class State : std::uint8_t { Empty, Full, Tombstone };

    struct Slot {
        K key{};
        V value{};
        State state = State::Empty;
    };

    static constexpr std::size_t NONE = static_cast<std::size_t>(-1);

    // Index of the Full slot holding `key`, or NONE. Skips tombstones and stops
    // at the first Empty slot.
    std::size_t locate(const K& key) const {
        const std::size_t mask = capacity() - 1;
        std::size_t i = hasher_(key) & mask;
        while (slots_[i].state != State::Empty) {
            if (slots_[i].state == State::Full && slots_[i].key == key) return i;
            i = (i + 1) & mask;
        }
        return NONE;
    }

    // Called when one more insert would exceed load factor 0.7. If live
    // entries alone fill more than half of that budget, double the capacity;
    // otherwise the table is mostly tombstones, so rebuild at the same size.
    void grow() {
        std::size_t new_cap = capacity();
        if ((size_ + 1) * 20 > capacity() * 7) {
            if (new_cap > (static_cast<std::size_t>(-1) >> 1)) throw std::length_error("HashMap too large");
            new_cap *= 2;
        }
        rebuild(new_cap);
    }

    // Re-inserts every live entry into a fresh slot array; tombstones vanish.
    void rebuild(std::size_t new_cap) {
        std::vector<Slot> old(new_cap);
        old.swap(slots_);
        size_ = 0;
        tombstones_ = 0;
        const std::size_t mask = new_cap - 1;
        for (Slot& s : old) {
            if (s.state != State::Full) continue;
            std::size_t i = hasher_(s.key) & mask;
            while (slots_[i].state != State::Empty) i = (i + 1) & mask;
            slots_[i] = s;
            ++size_;
        }
    }

    std::vector<Slot> slots_;
    std::size_t size_ = 0;
    std::size_t tombstones_ = 0;
    Hasher hasher_{};
};

} // namespace aletheia
