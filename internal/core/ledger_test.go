package core

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func acct(b byte) Hash {
	var h Hash
	h[0] = b
	return h
}

var (
	alice = acct(1)
	bob   = acct(2)
	carol = acct(3)
)

func newTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := NewLedger(Header{Version: 1, Timestamp: 1}, []Alloc{{alice, 1000}, {bob, 500}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// nextBlock builds a correctly linked block with the matching Merkle root.
func nextBlock(t *testing.T, l *Ledger, txs ...Tx) Block {
	t.Helper()
	_, tip, err := l.Tip()
	if err != nil {
		t.Fatal(err)
	}
	return Block{Header: Header{
		Version:    1,
		Prev:       HeaderHash(tip),
		MerkleRoot: MerkleRoot(TxIDs(txs)),
		Timestamp:  tip.Timestamp + 1,
	}, Txs: txs}
}

func pay(from, to Hash, amount, fee, seq uint64) Tx {
	return Tx{Sender: from, Recipient: to, Amount: amount, Fee: fee, Seq: seq}
}

func wantBalance(t *testing.T, l *Ledger, id Hash, bal, seq uint64) {
	t.Helper()
	b, s, err := l.Account(id)
	if err != nil || b != bal || s != seq {
		t.Fatalf("account %x: got (%d, %d, %v), want (%d, %d)", id[0], b, s, err, bal, seq)
	}
}

func TestNewLedgerArgs(t *testing.T) {
	if _, err := NewLedger(Header{Prev: acct(9)}, nil); err != ErrArg {
		t.Errorf("non-zero genesis prev: %v", err)
	}
	if _, err := NewLedger(Header{MerkleRoot: acct(9)}, nil); err != ErrArg {
		t.Errorf("non-zero genesis merkle root: %v", err)
	}
	if _, err := NewLedger(Header{}, []Alloc{{alice, 1}, {alice, 2}}); err != ErrArg {
		t.Errorf("duplicate allocation: %v", err)
	}
	l, err := NewLedger(Header{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}

func TestLedgerAppendAndQuery(t *testing.T) {
	l := newTestLedger(t)
	gen, _ := l.HeaderAt(0)
	if n, err := l.Height(); n != 0 || err != nil {
		t.Fatalf("Height = %d, %v", n, err)
	}
	if txs, err := l.BlockTxs(0); err != nil || len(txs) != 0 {
		t.Fatalf("genesis txs = %v, %v", txs, err)
	}

	t1 := pay(alice, bob, 100, 5, 0)
	t2 := pay(bob, carol, 50, 1, 0)
	b1 := nextBlock(t, l, t1, t2)
	if err := l.Append(b1); err != nil {
		t.Fatal(err)
	}
	t3 := pay(alice, carol, 10, 0, 1)
	b2 := nextBlock(t, l, t3)
	if err := l.Append(b2); err != nil {
		t.Fatal(err)
	}

	n, tip, err := l.Tip()
	if err != nil || n != 2 || tip != b2.Header {
		t.Fatalf("Tip = %d %+v %v", n, tip, err)
	}
	if h, _ := l.HeaderAt(1); h != b1.Header || h.Prev != HeaderHash(gen) {
		t.Fatal("HeaderAt(1) mismatch or not linked to genesis")
	}
	if _, err := l.HeaderAt(3); err != ErrNotFound {
		t.Fatalf("HeaderAt beyond tip: %v", err)
	}
	if txs, err := l.BlockTxs(1); err != nil || len(txs) != 2 || txs[0] != t1 || txs[1] != t2 {
		t.Fatalf("BlockTxs(1) = %v, %v", txs, err)
	}
	if _, err := l.BlockTxs(3); err != ErrNotFound {
		t.Fatalf("BlockTxs beyond tip: %v", err)
	}
	wantBalance(t, l, alice, 1000-105-10, 2)
	wantBalance(t, l, bob, 500+100-51, 1)
	wantBalance(t, l, carol, 60, 0)
	wantBalance(t, l, acct(42), 0, 0)

	if h, i, err := l.FindTx(TxID(t2)); err != nil || h != 1 || i != 1 {
		t.Fatalf("FindTx(t2) = %d %d %v", h, i, err)
	}
	if h, i, err := l.FindTx(TxID(t3)); err != nil || h != 2 || i != 0 {
		t.Fatalf("FindTx(t3) = %d %d %v", h, i, err)
	}
	if _, _, err := l.FindTx(acct(77)); err != ErrNotFound {
		t.Fatalf("FindTx(unknown) = %v", err)
	}

	// A proof built from the stored block verifies against the stored root:
	// the path a full node and light client will use.
	txs, _ := l.BlockTxs(1)
	p, err := MerkleProof(TxIDs(txs), 1)
	if err != nil || !MerkleVerify(b1.Header.MerkleRoot, TxID(t2), p) {
		t.Fatal("proof from stored block does not verify")
	}
}

// Each rule maps to its own error, and a rejected block changes nothing.
func TestLedgerRuleErrors(t *testing.T) {
	l := newTestLedger(t)
	ok := pay(alice, bob, 10, 1, 0)

	linkage := nextBlock(t, l, ok)
	linkage.Header.Prev[0] ^= 1
	merkle := nextBlock(t, l, ok)
	merkle.Header.MerkleRoot[0] ^= 1

	cases := []struct {
		name string
		b    Block
		want error
	}{
		{"linkage", linkage, ErrLinkage},
		{"merkle", merkle, ErrMerkle},
		{"empty", nextBlock(t, l), ErrTxSet},
		{"duplicate", nextBlock(t, l, ok, ok), ErrTxSet},
		{"balance", nextBlock(t, l, pay(alice, bob, 1000, 1, 0)), ErrBalance},
		{"seq", nextBlock(t, l, pay(alice, bob, 1, 0, 1)), ErrSeq},
		{"overflow", nextBlock(t, l, pay(alice, bob, ^uint64(0), 1, 0)), ErrOverflow},
		// One valid and one invalid transaction: nothing is applied.
		{"partial", nextBlock(t, l, ok, pay(bob, carol, 1, 0, 5)), ErrSeq},
	}
	for _, c := range cases {
		if err := l.Append(c.b); err != c.want {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	if n, _ := l.Height(); n != 0 {
		t.Fatalf("height %d after rejected blocks", n)
	}
	wantBalance(t, l, alice, 1000, 0)
	wantBalance(t, l, bob, 500, 0)
	if _, _, err := l.FindTx(TxID(ok)); err != ErrNotFound {
		t.Fatal("rejected transaction was indexed")
	}
}

func TestLedgerCheckTx(t *testing.T) {
	l := newTestLedger(t)
	if err := l.CheckTx(pay(alice, bob, 10, 1, 0)); err != nil {
		t.Errorf("valid: %v", err)
	}
	if err := l.CheckTx(pay(alice, bob, 1000, 1, 0)); err != ErrBalance {
		t.Errorf("balance: %v", err)
	}
	if err := l.CheckTx(pay(alice, bob, 1, 1, 1)); err != ErrSeq {
		t.Errorf("seq: %v", err)
	}
	if err := l.CheckTx(pay(alice, bob, ^uint64(0), 1, 0)); err != ErrOverflow {
		t.Errorf("overflow: %v", err)
	}
}

// DR-12: transactions that each pass CheckTx can conflict; CheckTxs finds the
// first conflicting one, and a list it accepts is accepted by Append.
func TestLedgerCheckTxs(t *testing.T) {
	l := newTestLedger(t)
	a := pay(alice, bob, 10, 1, 0)
	a2 := pay(alice, carol, 10, 1, 0) // same sender and seq as a
	if l.CheckTx(a) != nil || l.CheckTx(a2) != nil {
		t.Fatal("each should pass alone")
	}
	if bad, err := l.CheckTxs([]Tx{a, a2}); err != ErrSeq || bad != 1 {
		t.Errorf("same seq: (%d, %v)", bad, err)
	}
	if bad, err := l.CheckTxs([]Tx{a, a}); err != ErrTxSet || bad != 1 {
		t.Errorf("duplicate: (%d, %v)", bad, err)
	}
	if bad, err := l.CheckTxs([]Tx{pay(bob, carol, 1, 0, 3), a}); err != ErrSeq || bad != 0 {
		t.Errorf("first bad: (%d, %v)", bad, err)
	}
	if bad, err := l.CheckTxs(nil); err != nil || bad != -1 {
		t.Errorf("empty: (%d, %v)", bad, err)
	}
	// Chained transactions from one sender, in order, are fine.
	list := []Tx{a, pay(alice, carol, 5, 0, 1), pay(bob, carol, 509, 1, 0)}
	if bad, err := l.CheckTxs(list); err != nil || bad != -1 {
		t.Fatalf("valid list: (%d, %v)", bad, err)
	}
	if err := l.Append(nextBlock(t, l, list...)); err != nil {
		t.Fatalf("Append rejected a list CheckTxs accepted: %v", err)
	}
	if n, _ := l.Height(); n != 1 {
		t.Fatal("CheckTxs or Append mis-ordered")
	}
}

func TestLedgerClose(t *testing.T) {
	l, err := NewLedger(Header{}, []Alloc{{alice, 10}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	tx := pay(alice, bob, 1, 0, 0)
	errs := map[string]error{"Close": l.Close(), "CheckTx": l.CheckTx(tx), "Append": l.Append(Block{Txs: []Tx{tx}})}
	_, errs["Height"] = l.Height()
	_, errs["HeaderAt"] = l.HeaderAt(0)
	_, _, errs["Tip"] = l.Tip()
	_, errs["BlockTxs"] = l.BlockTxs(0)
	_, _, errs["Account"] = l.Account(alice)
	_, errs["CheckTxs"] = l.CheckTxs([]Tx{tx})
	_, _, errs["FindTx"] = l.FindTx(alice)
	for name, err := range errs {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("%s after Close: %v", name, err)
		}
	}
}

// 100 goroutines share one handle: readers (every read method) run while
// writers append. Under -race this checks the Go side; the C++ side is
// protected only by the lock, which the invariants below exercise: every
// append succeeds exactly once in some order, readers always see a
// consistent tip, and the final state equals the sequential result.
func TestLedgerConcurrent(t *testing.T) {
	const writers, readers, perWriter = 10, 90, 20
	allocs := make([]Alloc, writers)
	for w := range allocs {
		allocs[w] = Alloc{acct(byte(10 + w)), 1_000_000}
	}
	l, err := NewLedger(Header{}, allocs)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	var appended atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			sender := acct(byte(10 + w))
			last := time.Now()
			for seq := uint64(0); seq < perWriter; {
				if time.Since(last) > 10*time.Second { // a broken Tip/HeaderHash would loop forever
					t.Errorf("writer %d: no successful append for 10s", w)
					return
				}
				// Build against the current tip; another writer may win the race,
				// in which case Append reports ErrLinkage and we rebuild.
				_, tip, err := l.Tip()
				if err != nil {
					t.Error(err)
					return
				}
				tx := pay(sender, carol, 1, 1, seq)
				b := Block{Header: Header{Prev: HeaderHash(tip), MerkleRoot: TxID(tx), Nonce: uint64(w)}, Txs: []Tx{tx}}
				switch err := l.Append(b); err {
				case nil:
					appended.Add(1)
					seq++
					last = time.Now()
				case ErrLinkage:
				default:
					t.Error(err)
					return
				}
			}
		}(w)
	}
	stop := make(chan struct{})
	var rg sync.WaitGroup
	for r := 0; r < readers; r++ {
		rg.Add(1)
		go func(r int) {
			defer rg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				n, tip, err := l.Tip()
				if err != nil {
					t.Error(err)
					return
				}
				if n > 0 {
					txs, err := l.BlockTxs(n)
					if err != nil || len(txs) != 1 || TxID(txs[0]) != tip.MerkleRoot {
						t.Errorf("block %d inconsistent with its header: %v", n, err)
						return
					}
					if h, _, err := l.FindTx(tip.MerkleRoot); err != nil || h != n {
						t.Errorf("FindTx of tip tx: %d %v", h, err)
						return
					}
				}
				if _, _, err := l.Account(acct(byte(10 + r%writers))); err != nil {
					t.Error(err)
					return
				}
				l.CheckTx(pay(acct(byte(10+r%writers)), carol, 1, 1, uint64(i)))
				l.CheckTxs([]Tx{pay(acct(byte(10+r%writers)), carol, 1, 1, uint64(i))})
			}
		}(r)
	}
	wg.Wait()
	close(stop)
	rg.Wait()

	if got := appended.Load(); got != writers*perWriter {
		t.Fatalf("appended %d blocks, want %d", got, writers*perWriter)
	}
	if n, _ := l.Height(); n != writers*perWriter {
		t.Fatalf("height %d, want %d", n, writers*perWriter)
	}
	for w := 0; w < writers; w++ {
		wantBalance(t, l, acct(byte(10+w)), 1_000_000-2*perWriter, perWriter)
	}
	wantBalance(t, l, carol, writers*perWriter, 0)
}

// Close racing with readers: each reader either completes or gets ErrClosed;
// none touches freed memory (Close waits for in-flight calls).
func TestLedgerCloseWhileReading(t *testing.T) {
	l, err := NewLedger(Header{}, []Alloc{{alice, 10}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for r := 0; r < 50; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				_, _, err := l.Tip()
				if err == ErrClosed {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}
