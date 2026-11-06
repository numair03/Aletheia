package core

import (
	"math/bits"
	"testing"
)

// Reference values are the ones used by D1's C++ tests (cpp/tests/test_crypto.cpp),
// which were computed independently with Python's hashlib.

func mustParse(t *testing.T, s string) Hash {
	t.Helper()
	h, err := ParseHash(s)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// pattern(n)[i] = i % 251, as in test_crypto.cpp.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// leaves(n)[i] = SHA-256 of the single byte i, as in test_crypto.cpp.
func leaves(n int) []Hash {
	out := make([]Hash, n)
	for i := range out {
		out[i] = SHA256([]byte{byte(i)})
	}
	return out
}

func TestSHA256Vectors(t *testing.T) {
	cases := []struct {
		in   []byte
		want string
	}{
		{nil, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{[]byte{}, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{[]byte("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{[]byte("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq"),
			"248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1"},
		{pattern(55), "463eb28e72f82e0a96c0a4cc53690c571281131f672aa229e0d45ae59b598b59"},
		{pattern(56), "da2ae4d6b36748f2a318f23e7ab1dfdf45acdc9d049bd80e59de82a60895f562"},
		{pattern(64), "fdeab9acf3710362bd2658cdc9a29e8f9c757fcf9811603a8c447cd1d9151108"},
		{pattern(120), "f52b23db1fbb6ded89ef42a23ce0c8922c45f25c50b568a93bf1c075420bbb7c"},
	}
	for _, c := range cases {
		if got := SHA256(c.in).String(); got != c.want {
			t.Errorf("SHA256(len %d) = %s, want %s", len(c.in), got, c.want)
		}
	}
}

func TestParseHash(t *testing.T) {
	h := SHA256([]byte("abc"))
	if back, err := ParseHash(h.String()); err != nil || back != h {
		t.Fatalf("round trip: %v %v", back, err)
	}
	if _, err := ParseHash("BA7816BF8F01CFEA414140DE5DAE2223B00361A396177A9CB410FF61F20015AD"); err != nil {
		t.Error("upper case rejected")
	}
	for _, bad := range []string{"", "00", h.String()[:63], h.String() + "0", "zz" + h.String()[2:]} {
		if _, err := ParseHash(bad); err == nil {
			t.Errorf("ParseHash(%q) accepted", bad)
		}
	}
}

func TestMerkleKnownRoots(t *testing.T) {
	cases := map[int]string{
		1: "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d",
		2: "30e1867424e66e8b6d159246db94e3486778136f7e386ff5f001859d6b8484ab",
		3: "f2dcdd96791b6bac5d554f2d320e594b834f5da1981812c3707e7772234cb0ad",
		4: "9675e04b4ba9dc81b06e81731e2d21caa2c95557a85dcfa3fff70c9ff0f30b2e",
		5: "9674600fd139741c0f7dd7a32d984a0e74401cc90e6e8e5d203ed973d27324fe",
		7: "e263b77a6d80c1c56f3f67d1e0d803ad8eb2ac9d66c82f78735207c886a1592c",
		8: "0727b310f87099c1ba2ec0ba408def82c308237c8577f0bdfd2643e9cc6b7578",
	}
	for n, want := range cases {
		if got := MerkleRoot(leaves(n)); got != mustParse(t, want) {
			t.Errorf("root(%d) = %s, want %s", n, got, want)
		}
	}
	if MerkleRoot(nil) != (Hash{}) {
		t.Error("empty tree root must be zero (M-R5)")
	}
}

// Every proof of every leaf verifies, has ⌈log₂ n⌉ steps, and fails if the
// leaf, the root, any sibling or any side is changed.
func TestMerkleProofs(t *testing.T) {
	for n := 1; n <= 33; n++ {
		ls := leaves(n)
		root := MerkleRoot(ls)
		wantSteps := bits.Len(uint(n - 1)) // ⌈log₂ n⌉, 0 for n = 1
		for i := range ls {
			p, err := MerkleProof(ls, uint64(i))
			if err != nil {
				t.Fatalf("n=%d i=%d: %v", n, i, err)
			}
			if len(p) != wantSteps {
				t.Fatalf("n=%d i=%d: %d steps, want %d", n, i, len(p), wantSteps)
			}
			if !MerkleVerify(root, ls[i], p) {
				t.Fatalf("n=%d i=%d: valid proof rejected", n, i)
			}
			if MerkleVerify(root, SHA256([]byte("other")), p) {
				t.Fatalf("n=%d i=%d: wrong leaf accepted", n, i)
			}
			bad := root
			bad[0] ^= 1
			if MerkleVerify(bad, ls[i], p) {
				t.Fatalf("n=%d i=%d: wrong root accepted", n, i)
			}
			for s := range p {
				q := append(Proof(nil), p...)
				q[s].Sibling[31] ^= 1
				if MerkleVerify(root, ls[i], q) {
					t.Fatalf("n=%d i=%d: tampered sibling %d accepted", n, i, s)
				}
				q = append(Proof(nil), p...)
				q[s].Side ^= 1
				// Flipping a side changes the result unless the sibling equals the
				// running hash (the duplicated last node of an odd level).
				if MerkleVerify(root, ls[i], q) && q[s].Sibling != runningHash(ls[i], p[:s]) {
					t.Fatalf("n=%d i=%d: flipped side %d accepted", n, i, s)
				}
			}
		}
	}
}

// runningHash folds the first steps of a proof over leaf (test helper).
func runningHash(leaf Hash, steps Proof) Hash {
	h := leaf
	for _, s := range steps {
		if s.Side == Left {
			h = SHA256(append(s.Sibling[:], h[:]...))
		} else {
			h = SHA256(append(h[:], s.Sibling[:]...))
		}
	}
	return h
}

func TestMerkleMalformedProofs(t *testing.T) {
	ls := leaves(4)
	root := MerkleRoot(ls)
	p, _ := MerkleProof(ls, 1)

	q := append(Proof(nil), p...)
	q[0].Side = 2
	if MerkleVerify(root, ls[1], q) {
		t.Error("side 2 accepted")
	}
	long := make(Proof, MaxProofSteps+1)
	if MerkleVerify(root, ls[1], long) {
		t.Error("over-long proof accepted")
	}
	// A one-leaf tree: the leaf is its own root, with an empty proof (M-R4).
	if !MerkleVerify(ls[0], ls[0], nil) || MerkleVerify(ls[1], ls[0], nil) {
		t.Error("empty proof handling")
	}
}

func TestMerkleProofArgs(t *testing.T) {
	if _, err := MerkleProof(nil, 0); err != ErrArg {
		t.Errorf("empty list: %v", err)
	}
	if _, err := MerkleProof(leaves(3), 3); err != ErrArg {
		t.Errorf("index out of range: %v", err)
	}
}
