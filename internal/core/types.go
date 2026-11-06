package core

/*
#include "ledger_c.h"
*/
import "C"

// Sizes of the canonical encodings (ledger_c.h).
const (
	TxSize     = C.AL_TX_SIZE
	HeaderSize = C.AL_HEADER_SIZE
)

// Tx is a transfer of Amount from Sender to Recipient, paying Fee (burned).
// Seq must equal the sender's next sequence number.
type Tx struct {
	Sender    Hash
	Recipient Hash
	Amount    uint64
	Fee       uint64
	Seq       uint64
}

// Header is a block header. Difficulty is the required number of leading zero
// bits of the block hash; the ledger does not check it (consensus does).
type Header struct {
	Version    uint32
	Prev       Hash
	MerkleRoot Hash
	Timestamp  int64
	Difficulty uint32
	Nonce      uint64
}

// Alloc is a genesis balance.
type Alloc struct {
	Account Hash
	Balance uint64
}

// Block is a header and its transactions, in order.
type Block struct {
	Header Header
	Txs    []Tx
}

// Field-by-field copies between Go and C structs. Go never lays out the
// canonical bytes itself; the single encoding lives in D1's block.cpp.

func hashToC(h Hash) (c [32]C.uint8_t) {
	for i, b := range h {
		c[i] = C.uint8_t(b)
	}
	return c
}

func hashFromC(c [32]C.uint8_t) (h Hash) {
	for i, b := range c {
		h[i] = byte(b)
	}
	return h
}

func (t Tx) toC() C.al_tx {
	return C.al_tx{
		sender:    hashToC(t.Sender),
		recipient: hashToC(t.Recipient),
		amount:    C.uint64_t(t.Amount),
		fee:       C.uint64_t(t.Fee),
		seq:       C.uint64_t(t.Seq),
	}
}

func txFromC(c *C.al_tx) Tx {
	return Tx{
		Sender:    hashFromC(c.sender),
		Recipient: hashFromC(c.recipient),
		Amount:    uint64(c.amount),
		Fee:       uint64(c.fee),
		Seq:       uint64(c.seq),
	}
}

func (h Header) toC() C.al_header {
	return C.al_header{
		version:     C.uint32_t(h.Version),
		prev:        hashToC(h.Prev),
		merkle_root: hashToC(h.MerkleRoot),
		timestamp:   C.int64_t(h.Timestamp),
		difficulty:  C.uint32_t(h.Difficulty),
		nonce:       C.uint64_t(h.Nonce),
	}
}

func headerFromC(c *C.al_header) Header {
	return Header{
		Version:    uint32(c.version),
		Prev:       hashFromC(c.prev),
		MerkleRoot: hashFromC(c.merkle_root),
		Timestamp:  int64(c.timestamp),
		Difficulty: uint32(c.difficulty),
		Nonce:      uint64(c.nonce),
	}
}

// txsToC copies a transaction list into C structs for one call.
func txsToC(txs []Tx) []C.al_tx {
	c := make([]C.al_tx, len(txs))
	for i, t := range txs {
		c[i] = t.toC()
	}
	return c
}

// txsPtr returns a pointer to the first element, or nil for an empty list.
func txsPtr(c []C.al_tx) *C.al_tx {
	if len(c) == 0 {
		return nil
	}
	return &c[0]
}

// EncodeTx returns the canonical 88-byte encoding (big-endian fields).
func EncodeTx(t Tx) [TxSize]byte {
	c := t.toC()
	var out [TxSize]byte
	must(C.al_tx_encode(&c, (*C.uint8_t)(&out[0])))
	return out
}

// DecodeTx is the inverse of EncodeTx; every 88-byte string decodes.
func DecodeTx(b [TxSize]byte) Tx {
	var c C.al_tx
	must(C.al_tx_decode((*C.uint8_t)(&b[0]), &c))
	return txFromC(&c)
}

// TxID is SHA-256 of the canonical encoding: the transaction's id and its
// Merkle leaf.
func TxID(t Tx) Hash {
	c := t.toC()
	var out Hash
	must(C.al_tx_id(&c, (*C.uint8_t)(&out[0])))
	return out
}

// TxIDs returns the txid of every transaction, in order (the Merkle leaves).
func TxIDs(txs []Tx) []Hash {
	ids := make([]Hash, len(txs))
	for i, t := range txs {
		ids[i] = TxID(t)
	}
	return ids
}

// EncodeHeader returns the canonical 88-byte header encoding.
func EncodeHeader(h Header) [HeaderSize]byte {
	c := h.toC()
	var out [HeaderSize]byte
	must(C.al_header_encode(&c, (*C.uint8_t)(&out[0])))
	return out
}

// DecodeHeader is the inverse of EncodeHeader.
func DecodeHeader(b [HeaderSize]byte) Header {
	var c C.al_header
	must(C.al_header_decode((*C.uint8_t)(&b[0]), &c))
	return headerFromC(&c)
}

// HeaderHash is the block hash: SHA-256 of the canonical header encoding.
func HeaderHash(h Header) Hash {
	c := h.toC()
	var out Hash
	must(C.al_header_hash(&c, (*C.uint8_t)(&out[0])))
	return out
}
