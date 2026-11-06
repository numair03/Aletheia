package core

import (
	"encoding/hex"
	"math/rand"
	"strings"
	"testing"
)

// sampleTx and sampleHeader match cpp/tests/test_block.cpp, whose expected
// bytes and hashes were computed independently with Python struct + hashlib.
func sampleTx() Tx {
	var t Tx
	for i := 0; i < 32; i++ {
		t.Sender[i] = byte(i)
		t.Recipient[i] = byte(32 + i)
	}
	t.Amount = 1000
	t.Fee = 0x0102030405060708
	t.Seq = 0xFFFFFFFFFFFFFFFF
	return t
}

func sampleHeader() Header {
	var h Header
	h.Version = 1
	for i := range h.Prev {
		h.Prev[i] = 0xAA
		h.MerkleRoot[i] = 0xBB
	}
	h.Timestamp = -2
	h.Difficulty = 20
	h.Nonce = 0xDEADBEEF
	return h
}

func TestTxEncodingVector(t *testing.T) {
	tx := sampleTx()
	b := EncodeTx(tx)
	want := hex.EncodeToString(tx.Sender[:]) + hex.EncodeToString(tx.Recipient[:]) +
		"00000000000003e8" + "0102030405060708" + "ffffffffffffffff"
	if got := hex.EncodeToString(b[:]); got != want {
		t.Fatalf("encoding\n got %s\nwant %s", got, want)
	}
	if got := TxID(tx).String(); got != "5c242f353249fbaed47cf55c805566e1f96567fb26b6de936bbf00c4c960331e" {
		t.Fatalf("txid = %s", got)
	}
	if TxID(tx) != SHA256(b[:]) {
		t.Fatal("txid is not SHA-256 of the encoding")
	}
	if DecodeTx(b) != tx {
		t.Fatal("decode(encode(tx)) != tx")
	}
}

func TestHeaderEncodingVector(t *testing.T) {
	h := sampleHeader()
	b := EncodeHeader(h)
	want := "00000001" + strings.Repeat("a", 64) + strings.Repeat("b", 64) +
		"fffffffffffffffe" + "00000014" + "00000000deadbeef"
	if got := hex.EncodeToString(b[:]); got != want {
		t.Fatalf("encoding\n got %s\nwant %s", got, want)
	}
	if got := HeaderHash(h).String(); got != "157d2701231da08ba62f42d5b157c6dc65aa9d6d93926bd3e6c2f390ee113b15" {
		t.Fatalf("header hash = %s", got)
	}
	if DecodeHeader(b) != h {
		t.Fatal("decode(encode(h)) != h")
	}
	if got := HeaderHash(Header{}).String(); got != "10eef285deef7a4b7c82b22aa53589b7833df29de3814649c772bbd5c832f365" {
		t.Fatalf("zero header hash = %s", got)
	}
}

// Every field reaches the C struct: changing any one changes the hash. This
// catches a field dropped by toC.
func TestEveryFieldHashed(t *testing.T) {
	tx := sampleTx()
	id := TxID(tx)
	muts := []func(*Tx){
		func(t *Tx) { t.Sender[5] ^= 1 }, func(t *Tx) { t.Recipient[31] ^= 1 },
		func(t *Tx) { t.Amount ^= 1 }, func(t *Tx) { t.Fee ^= 1 }, func(t *Tx) { t.Seq ^= 1 },
	}
	for i, m := range muts {
		c := tx
		m(&c)
		if TxID(c) == id {
			t.Errorf("tx field %d not hashed", i)
		}
	}
	h := sampleHeader()
	hh := HeaderHash(h)
	hmuts := []func(*Header){
		func(h *Header) { h.Version ^= 1 }, func(h *Header) { h.Prev[0] ^= 1 },
		func(h *Header) { h.MerkleRoot[31] ^= 1 }, func(h *Header) { h.Timestamp ^= 1 },
		func(h *Header) { h.Difficulty ^= 1 }, func(h *Header) { h.Nonce ^= 1 },
	}
	for i, m := range hmuts {
		c := h
		m(&c)
		if HeaderHash(c) == hh {
			t.Errorf("header field %d not hashed", i)
		}
	}
}

// Arbitrary bytes round-trip through decode then encode, so the Go<->C copies
// lose nothing in any field.
func TestRandomRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		var tb [TxSize]byte
		var hb [HeaderSize]byte
		r.Read(tb[:])
		r.Read(hb[:])
		if EncodeTx(DecodeTx(tb)) != tb {
			t.Fatalf("tx round trip failed for %x", tb)
		}
		if EncodeHeader(DecodeHeader(hb)) != hb {
			t.Fatalf("header round trip failed for %x", hb)
		}
	}
}

func TestTxIDs(t *testing.T) {
	a, b := sampleTx(), sampleTx()
	b.Seq = 0
	ids := TxIDs([]Tx{a, b})
	if len(ids) != 2 || ids[0] != TxID(a) || ids[1] != TxID(b) {
		t.Fatal("TxIDs mismatch")
	}
	if len(TxIDs(nil)) != 0 {
		t.Fatal("TxIDs(nil) not empty")
	}
}
