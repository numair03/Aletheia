#pragma once

#include "aletheia/types.h"

#include <array>
#include <cstddef>
#include <cstdint>
#include <vector>

namespace aletheia {

// A transfer of `amount` from `sender` to `recipient`. `fee` orders the
// mempool and is burned (there is no mining reward). `seq` is the sender's
// next sequence number and makes every transaction unique (replay protection).
// Account IDs are opaque 32-byte identifiers; there are no signatures.
struct Transaction {
    Hash256 sender{};
    Hash256 recipient{};
    std::uint64_t amount = 0;
    std::uint64_t fee = 0;
    std::uint64_t seq = 0;
};

// 88-byte block header. The block hash is SHA-256 of its canonical encoding,
// and `difficulty` is the required number of leading zero bits of that hash.
struct BlockHeader {
    std::uint32_t version = 0;
    Hash256 prev{};         // hash of the previous header (all zero for genesis)
    Hash256 merkle_root{};  // Merkle root over the block's txids
    std::int64_t timestamp = 0;
    std::uint32_t difficulty = 0;
    std::uint64_t nonce = 0;
};

struct Block {
    BlockHeader header;
    std::vector<Transaction> txs;
};

bool operator==(const Transaction& a, const Transaction& b);
bool operator!=(const Transaction& a, const Transaction& b);
bool operator==(const BlockHeader& a, const BlockHeader& b);
bool operator!=(const BlockHeader& a, const BlockHeader& b);

// Canonical encodings: the only byte layout that is ever hashed. Big-endian.
//   tx     = sender[32] | recipient[32] | amount u64 | fee u64 | seq u64
//   header = version u32 | prev[32] | merkle_root[32] | timestamp i64 |
//            difficulty u32 | nonce u64
// Every 88-byte input decodes to some value, and decode(encode(x)) == x.
constexpr std::size_t TX_SIZE = 88;
constexpr std::size_t HEADER_SIZE = 88;
using TxBytes = std::array<std::uint8_t, TX_SIZE>;
using HeaderBytes = std::array<std::uint8_t, HEADER_SIZE>;

TxBytes encode(const Transaction& tx);
HeaderBytes encode(const BlockHeader& h);
Transaction decode_tx(const std::uint8_t* in);     // reads TX_SIZE bytes
BlockHeader decode_header(const std::uint8_t* in); // reads HEADER_SIZE bytes

// txid = SHA-256(encode(tx)); block hash = SHA-256(encode(header)).
Hash256 tx_id(const Transaction& tx);
Hash256 header_hash(const BlockHeader& h);

// The txids of `txs`, in order: the leaves of the block's Merkle tree (M-R1).
std::vector<Hash256> tx_ids(const std::vector<Transaction>& txs);

} // namespace aletheia
