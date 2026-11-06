package mempool

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anon-HP/Aletheia/internal/core"
)

func acct(i int) core.Hash {
	var h core.Hash
	h[0], h[1] = byte(i>>8), byte(i)
	h[31] = 0xA1 // never the zero hash
	return h
}

func pay(from, to core.Hash, amount, fee, seq uint64) core.Tx {
	return core.Tx{Sender: from, Recipient: to, Amount: amount, Fee: fee, Seq: seq}
}

var sink = acct(9999)

// newLedger creates a ledger in which accounts 0..n-1 each hold balance.
func newLedger(t *testing.T, n int, balance uint64) *core.Ledger {
	t.Helper()
	allocs := make([]core.Alloc, n)
	for i := range allocs {
		allocs[i] = core.Alloc{Account: acct(i), Balance: balance}
	}
	l, err := core.NewLedger(core.Header{Version: 1, Timestamp: 1}, allocs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func newPool(t *testing.T, l *core.Ledger) *Pool {
	t.Helper()
	p, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// appendBlock builds a correctly linked block with the matching Merkle root
// on l's current tip and appends it.
func appendBlock(l *core.Ledger, txs []core.Tx) error {
	_, tip, err := l.Tip()
	if err != nil {
		return err
	}
	return l.Append(core.Block{Header: core.Header{
		Version:    1,
		Prev:       core.HeaderHash(tip),
		MerkleRoot: core.MerkleRoot(core.TxIDs(txs)),
		Timestamp:  tip.Timestamp + 1,
	}, Txs: txs})
}

func mustSubmit(t *testing.T, p *Pool, tx core.Tx) {
	t.Helper()
	if _, err := p.Submit(tx); err != nil {
		t.Fatalf("Submit(%+v): %v", tx, err)
	}
}

func pending(t *testing.T, p *Pool) []core.Tx {
	t.Helper()
	txs, err := p.Pending()
	if err != nil {
		t.Fatal(err)
	}
	return txs
}

func TestNewArgs(t *testing.T) {
	if _, err := New(nil); err != core.ErrArg {
		t.Fatalf("New(nil): %v", err)
	}
}

func TestSubmitAdmission(t *testing.T) {
	l := newLedger(t, 3, 100)
	p := newPool(t, l)
	a, b, c := acct(0), acct(1), acct(2)

	tx := pay(a, b, 10, 1, 0)
	id, err := p.Submit(tx)
	if err != nil || id != core.TxID(tx) {
		t.Fatalf("Submit = %s, %v", id, err)
	}
	cases := []struct {
		name string
		tx   core.Tx
		want error
	}{
		{"same transaction again", tx, ErrDuplicate},
		{"same sender and seq, other recipient", pay(a, c, 10, 1, 0), ErrSenderPending},
		{"sender's next seq while one is pending", pay(a, b, 10, 1, 1), ErrSenderPending},
		{"wrong seq", pay(b, a, 1, 1, 5), core.ErrSeq},
		{"amount + fee above balance", pay(b, a, 100, 1, 0), core.ErrBalance},
		{"amount + fee overflows", pay(b, a, math.MaxUint64, 1, 0), core.ErrOverflow},
		{"unknown sender", pay(acct(50), a, 1, 0, 0), core.ErrBalance},
	}
	for _, tc := range cases {
		if _, err := p.Submit(tc.tx); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	if p.Len() != 1 {
		t.Fatalf("Len = %d after rejections, want 1", p.Len())
	}
	mustSubmit(t, p, pay(b, a, 99, 1, 0)) // exactly the whole balance
	if p.Len() != 2 {
		t.Fatalf("Len = %d, want 2", p.Len())
	}
}

// Pending and Batch follow the fee queue's order through the Go API: fee
// descending, then txid ascending.
func TestFeeOrder(t *testing.T) {
	const n = 40
	l := newLedger(t, n, 1000)
	p := newPool(t, l)
	var in []core.Tx
	for i := 0; i < n; i++ {
		tx := pay(acct(i), sink, 1, uint64((i*7)%5), 0) // many equal fees
		in = append(in, tx)
		mustSubmit(t, p, tx)
	}
	sort.Slice(in, func(i, j int) bool {
		if in[i].Fee != in[j].Fee {
			return in[i].Fee > in[j].Fee
		}
		a, b := core.TxID(in[i]), core.TxID(in[j])
		return bytes.Compare(a[:], b[:]) < 0
	})
	got := pending(t, p)
	if len(got) != n {
		t.Fatalf("Pending returned %d, want %d", len(got), n)
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("Pending[%d] = %+v, want %+v", i, got[i], in[i])
		}
	}
	batch, err := p.Batch(7)
	if err != nil || len(batch) != 7 {
		t.Fatalf("Batch(7) = %d txs, %v", len(batch), err)
	}
	for i := range batch {
		if batch[i] != in[i] {
			t.Fatalf("Batch[%d] = %+v, want %+v", i, batch[i], in[i])
		}
	}
	// Batch and Pending leave the pool unchanged.
	if p.Len() != n || len(pending(t, p)) != n {
		t.Fatalf("pool changed: Len %d", p.Len())
	}
	if b, err := p.Batch(0); b != nil || err != nil {
		t.Fatalf("Batch(0) = %v, %v", b, err)
	}
	if b, _ := p.Batch(1000); len(b) != n {
		t.Fatalf("Batch(1000) = %d txs, want %d", len(b), n)
	}
}

// A batch is a block the ledger accepts, even when a pending transaction has
// gone stale (DR-12): the stale one is skipped, not included.
func TestBatchSkipsInvalid(t *testing.T) {
	l := newLedger(t, 4, 100)
	p := newPool(t, l)
	for i := 0; i < 4; i++ {
		mustSubmit(t, p, pay(acct(i), sink, 10, uint64(10+i), 0))
	}
	// Behind the pool's back, a block confirms a different seq-0 transaction
	// of the highest-fee sender (acct 3), so its pending one is now invalid.
	if err := appendBlock(l, []core.Tx{pay(acct(3), acct(0), 5, 0, 0)}); err != nil {
		t.Fatal(err)
	}
	batch, err := p.Batch(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 3 {
		t.Fatalf("Batch kept %d, want 3 (stale one skipped)", len(batch))
	}
	for _, tx := range batch {
		if tx.Sender == acct(3) {
			t.Fatal("stale transaction in the batch")
		}
	}
	if err := appendBlock(l, batch); err != nil {
		t.Fatalf("ledger rejected the assembled batch: %v", err)
	}
	if p.Len() != 4 {
		t.Fatalf("Batch removed transactions: Len %d", p.Len())
	}
}

func TestWaitNonEmpty(t *testing.T) {
	l := newLedger(t, 2, 100)
	p := newPool(t, l)

	// Wakes on Submit.
	woke := make(chan error, 1)
	go func() { woke <- p.WaitNonEmpty(context.Background()) }()
	select {
	case err := <-woke:
		t.Fatalf("returned on an empty pool: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	mustSubmit(t, p, pay(acct(0), sink, 1, 1, 0))
	select {
	case err := <-woke:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitNonEmpty did not wake on Submit")
	}
	// Returns at once when non-empty.
	if err := p.WaitNonEmpty(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Returns on cancellation.
	empty := newPool(t, l)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { woke <- empty.WaitNonEmpty(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-woke:
		if err != context.Canceled {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitNonEmpty ignored cancellation")
	}
	// An already-cancelled context on an empty pool returns at once.
	if err := empty.WaitNonEmpty(ctx); err != context.Canceled {
		t.Fatalf("got %v", err)
	}

	// Returns on Close.
	go func() { woke <- empty.WaitNonEmpty(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	empty.Close()
	select {
	case err := <-woke:
		if err != ErrClosed {
			t.Fatalf("got %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitNonEmpty did not return on Close")
	}
}

// After a block: the included transactions are gone, a pending one the block
// made stale is evicted, the rest stay, and senders can submit their next seq.
func TestReconcileAfterBlock(t *testing.T) {
	l := newLedger(t, 4, 100)
	p := newPool(t, l)
	a, b, c, d := acct(0), acct(1), acct(2), acct(3)
	ta, tb, tc, td := pay(a, sink, 10, 3, 0), pay(b, sink, 10, 2, 0), pay(c, sink, 10, 1, 0), pay(d, sink, 10, 4, 0)
	for _, tx := range []core.Tx{ta, tb, tc, td} {
		mustSubmit(t, p, tx)
	}
	// The block holds ta, td and a different seq-0 transaction of c (from
	// another node), which makes tc stale. tb was not included.
	block := []core.Tx{ta, td, pay(c, a, 95, 5, 0)}
	if err := appendBlock(l, block); err != nil {
		t.Fatal(err)
	}
	evicted, readded, err := p.Reconcile(l, nil)
	if err != nil || evicted != 3 || readded != 0 {
		t.Fatalf("Reconcile = %d, %d, %v; want 3, 0", evicted, readded, err)
	}
	if got := pending(t, p); len(got) != 1 || got[0] != tb || p.Len() != 1 {
		t.Fatalf("pending = %+v, Len %d; want only tb", got, p.Len())
	}
	// a's next transaction is admissible now; tb is still a duplicate.
	mustSubmit(t, p, pay(a, sink, 1, 1, 1))
	if _, err := p.Submit(tb); err != ErrDuplicate {
		t.Fatalf("resubmitted tb: %v", err)
	}
	// Nothing to do on a second reconcile.
	if e, r, err := p.Reconcile(l, nil); e != 0 || r != 0 || err != nil {
		t.Fatalf("idle Reconcile = %d, %d, %v", e, r, err)
	}
}

// After a chain switch: the pool checks against the new ledger, transactions
// of abandoned blocks come back if still valid, and pending ones that the new
// chain invalidates are evicted.
func TestReconcileChainSwitch(t *testing.T) {
	a, b, c := acct(0), acct(1), acct(2)
	old := newLedger(t, 3, 100)
	p := newPool(t, old)

	x := pay(a, sink, 10, 1, 0) // confirmed on the old chain only
	if err := appendBlock(old, []core.Tx{x}); err != nil {
		t.Fatal(err)
	}
	z := pay(a, sink, 10, 1, 1) // a's next on the old chain
	w := pay(b, sink, 10, 1, 0) // valid on both chains
	mustSubmit(t, p, z)
	mustSubmit(t, p, w)

	// The new chain (replayed from genesis) holds a block of c only.
	repl := newLedger(t, 3, 100)
	y := pay(c, sink, 10, 1, 0)
	if err := appendBlock(repl, []core.Tx{y}); err != nil {
		t.Fatal(err)
	}
	// Abandoned: x. Readding y too checks that a transaction the new chain
	// already confirmed is not readmitted.
	evicted, readded, err := p.Reconcile(repl, []core.Tx{x, y})
	if err != nil || evicted != 1 || readded != 1 {
		t.Fatalf("Reconcile = %d, %d, %v; want 1 (z), 1 (x)", evicted, readded, err)
	}
	got := pending(t, p)
	if len(got) != 2 || p.Len() != 2 {
		t.Fatalf("pending = %+v", got)
	}
	for _, tx := range got {
		if tx != x && tx != w {
			t.Fatalf("unexpected pending %+v", tx)
		}
	}
	// The old ledger can be closed; the pool no longer uses it.
	old.Close()
	if _, err := p.Submit(y); !errors.Is(err, core.ErrSeq) {
		t.Fatalf("submit against the new ledger: %v", err)
	}
	if b, err := p.Batch(5); err != nil || len(b) != 2 {
		t.Fatalf("Batch after switch = %d, %v", len(b), err)
	}
	// Reconciling with a closed ledger fails and changes nothing.
	if _, _, err := p.Reconcile(old, nil); err != core.ErrClosed {
		t.Fatalf("Reconcile(closed) = %v", err)
	}
	if p.Len() != 2 {
		t.Fatalf("Len = %d after failed reconcile", p.Len())
	}
	if _, err := p.Batch(5); err != nil {
		t.Fatalf("pool still points at the closed ledger: %v", err)
	}
}

func TestClose(t *testing.T) {
	l := newLedger(t, 1, 100)
	p, err := New(l)
	if err != nil {
		t.Fatal(err)
	}
	mustSubmit(t, p, pay(acct(0), sink, 1, 1, 0))
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if p.Len() != 0 {
		t.Fatalf("Len = %d", p.Len())
	}
	if _, err := p.Submit(pay(acct(0), sink, 1, 1, 0)); err != ErrClosed {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := p.Batch(1); err != ErrClosed {
		t.Fatalf("Batch: %v", err)
	}
	if _, err := p.Pending(); err != ErrClosed {
		t.Fatalf("Pending: %v", err)
	}
	if _, _, err := p.Reconcile(l, nil); err != ErrClosed {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := p.WaitNonEmpty(context.Background()); err != ErrClosed {
		t.Fatalf("WaitNonEmpty: %v", err)
	}
	if err := p.Close(); err != ErrClosed {
		t.Fatalf("second Close: %v", err)
	}
}

// Race stress: many submitters (each with a chain of transactions, plus
// duplicate resubmissions), two miners that wait, assemble, append and
// reconcile concurrently, and readers. Run under -race. At the end every
// transaction was confirmed exactly once and the pool is empty; no batch
// ever held a duplicate or two transactions of one sender.
func TestRaceStress(t *testing.T) {
	const senders, perSender, miners, maxBatch = 64, 25, 3, 8
	l := newLedger(t, senders, 1_000_000)
	p := newPool(t, l)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var submitted, duplicates atomic.Int64
	var badBatch atomic.Value
	var wg sync.WaitGroup
	for s := 0; s < senders; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			deadline := time.Now().Add(60 * time.Second)
			for seq := uint64(0); seq < perSender; {
				if time.Now().After(deadline) {
					t.Errorf("sender %d stuck at seq %d", s, seq)
					return
				}
				tx := pay(acct(s), acct((s+1)%senders), 1, uint64((s+int(seq))%9), seq)
				_, err := p.Submit(tx)
				switch {
				case err == nil:
					submitted.Add(1)
					if _, err := p.Submit(tx); err == ErrDuplicate { // a client retry
						duplicates.Add(1)
					} else if !errors.Is(err, core.ErrSeq) { // ErrSeq: already mined
						t.Errorf("resubmit: %v", err)
						return
					}
					seq++
				case err == ErrSenderPending:
					time.Sleep(time.Millisecond) // wait for the previous one to be mined
				default:
					t.Errorf("sender %d seq %d: %v", s, seq, err)
					return
				}
			}
		}(s)
	}

	var blocks atomic.Int64
	var mwg sync.WaitGroup
	for m := 0; m < miners; m++ {
		mwg.Add(1)
		go func() {
			defer mwg.Done()
			for {
				if err := p.WaitNonEmpty(ctx); err != nil {
					return
				}
				batch, err := p.Batch(maxBatch)
				if err != nil {
					t.Error(err)
					return
				}
				ids, from := map[core.Hash]bool{}, map[core.Hash]bool{}
				for _, tx := range batch {
					if ids[core.TxID(tx)] || from[tx.Sender] {
						badBatch.Store(batch)
					}
					ids[core.TxID(tx)], from[tx.Sender] = true, true
				}
				if len(batch) == 0 {
					continue
				}
				// The other miner may have appended since Batch: then this
				// fails (linkage, seq or duplicate) and the next round retries.
				if appendBlock(l, batch) == nil {
					blocks.Add(1)
				}
				if _, _, err := p.Reconcile(l, nil); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}

	// Readers: Len stays within bounds (at most one pending per sender) and
	// Pending agrees with the one-per-sender rule.
	rctx, rstop := context.WithCancel(ctx)
	var rwg sync.WaitGroup
	for r := 0; r < 4; r++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for rctx.Err() == nil {
				if n := p.Len(); n < 0 || n > senders {
					t.Errorf("Len = %d", n)
					return
				}
				txs, err := p.Pending()
				if err != nil {
					t.Error(err)
					return
				}
				from := map[core.Hash]bool{}
				for _, tx := range txs {
					if from[tx.Sender] {
						t.Error("two pending transactions from one sender")
						return
					}
					from[tx.Sender] = true
				}
			}
		}()
	}

	wg.Wait()
	for deadline := time.Now().Add(30 * time.Second); p.Len() > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("pool not drained: Len %d", p.Len())
		}
	}
	rstop()
	cancel()
	mwg.Wait()
	rwg.Wait()

	if b := badBatch.Load(); b != nil {
		t.Fatalf("batch with a duplicate or a repeated sender: %+v", b)
	}
	if got := submitted.Load(); got != senders*perSender {
		t.Fatalf("submitted %d, want %d", got, senders*perSender)
	}
	// Every transaction confirmed exactly once: each sender's next seq is
	// perSender, and the chain holds exactly senders*perSender transactions.
	confirmed := 0
	h, _, _ := l.Tip()
	for i := uint64(1); i <= h; i++ {
		txs, err := l.BlockTxs(i)
		if err != nil {
			t.Fatal(err)
		}
		confirmed += len(txs)
	}
	if confirmed != senders*perSender {
		t.Fatalf("chain holds %d transactions, want %d", confirmed, senders*perSender)
	}
	for s := 0; s < senders; s++ {
		if _, seq, _ := l.Account(acct(s)); seq != perSender {
			t.Fatalf("sender %d next seq %d, want %d", s, seq, perSender)
		}
	}
	if p.Len() != 0 || len(pending(t, p)) != 0 {
		t.Fatalf("pool not empty: Len %d", p.Len())
	}
	t.Logf("%d blocks, %d duplicate resubmissions rejected", blocks.Load(), duplicates.Load())
}
