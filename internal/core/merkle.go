package core

/*
#include "crypto_c.h"
*/
import "C"

import (
	"encoding/hex"
	"errors"
	"unsafe"
)

// Hash is a 32-byte SHA-256 digest (txid, block hash, Merkle node, account id).
type Hash [32]byte

// String returns the hash as 64 lowercase hex digits.
func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// ParseHash parses exactly 64 hex digits (either case).
func ParseHash(s string) (Hash, error) {
	var h Hash
	if len(s) != 2*len(h) {
		return h, errors.New("core: hash must be 64 hex digits")
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return Hash{}, errors.New("core: hash must be 64 hex digits")
	}
	return h, nil
}

// MaxProofSteps is the longest Merkle proof the C++ core produces or accepts.
const MaxProofSteps = C.AL_MERKLE_MAX_STEPS

// Side says where a proof step's sibling sits relative to the running hash.
type Side uint8

const (
	Left  Side = 0 // parent = SHA-256(sibling || running)
	Right Side = 1 // parent = SHA-256(running || sibling)
)

// ProofStep is one level of an inclusion proof, leaf level first.
type ProofStep struct {
	Sibling Hash
	Side    Side
}

// Proof is a Merkle inclusion proof: ⌈log₂ n⌉ steps for a tree of n leaves.
type Proof []ProofStep

// bytePtr returns a C pointer to b's first byte, or nil for an empty slice.
// The memory stays Go-owned; C++ only reads it during the call.
func bytePtr(b []byte) *C.uint8_t {
	if len(b) == 0 {
		return nil
	}
	return (*C.uint8_t)(unsafe.Pointer(&b[0]))
}

// hashesPtr views a []Hash as n*32 contiguous bytes, which is how the C API
// takes a list of hashes ([32]byte has no padding, so the layouts agree).
func hashesPtr(hs []Hash) *C.uint8_t {
	if len(hs) == 0 {
		return nil
	}
	return (*C.uint8_t)(unsafe.Pointer(&hs[0][0]))
}

// SHA256 hashes data with the C++ implementation (the project's only SHA-256).
func SHA256(data []byte) Hash {
	var out Hash
	must(C.al_sha256(bytePtr(data), C.size_t(len(data)), (*C.uint8_t)(&out[0])))
	return out
}

// MerkleRoot returns the root of the tree over leaves; zero for no leaves.
func MerkleRoot(leaves []Hash) Hash {
	var out Hash
	must(C.al_merkle_root(hashesPtr(leaves), C.uint64_t(len(leaves)), (*C.uint8_t)(&out[0])))
	return out
}

// MerkleProof returns the inclusion proof of leaves[index].
// ErrArg if index is out of range (including an empty list).
func MerkleProof(leaves []Hash, index uint64) (Proof, error) {
	if index >= uint64(len(leaves)) {
		return nil, ErrArg
	}
	var hashes [MaxProofSteps]Hash
	var dirs [MaxProofSteps]byte
	var steps C.uint32_t
	st := C.al_merkle_proof(hashesPtr(leaves), C.uint64_t(len(leaves)), C.uint64_t(index),
		hashesPtr(hashes[:]), bytePtr(dirs[:]), MaxProofSteps, &steps)
	if err := statusErr(st); err != nil {
		return nil, err
	}
	p := make(Proof, steps)
	for i := range p {
		p[i] = ProofStep{Sibling: hashes[i], Side: Side(dirs[i])}
	}
	return p, nil
}

// MerkleVerify reports whether proof links leaf to root. A malformed proof
// (more than MaxProofSteps steps, or a Side other than Left or Right) is
// simply not valid, so this never fails with an error.
func MerkleVerify(root, leaf Hash, proof Proof) bool {
	if len(proof) > MaxProofSteps {
		return false
	}
	var hashes [MaxProofSteps]Hash
	var dirs [MaxProofSteps]byte
	for i, s := range proof {
		hashes[i] = s.Sibling
		dirs[i] = byte(s.Side)
	}
	st := C.al_merkle_verify((*C.uint8_t)(&root[0]), (*C.uint8_t)(&leaf[0]),
		hashesPtr(hashes[:]), bytePtr(dirs[:]), C.uint32_t(len(proof)))
	if st < 0 {
		panic(statusErr(st)) // only null pointers fail, and none are passed
	}
	return st == 1
}
