package client

import (
	"context"
	"errors"
	"fmt"

	"github.com/Anon-HP/Aletheia/internal/api"
	"github.com/Anon-HP/Aletheia/internal/consensus"
	"github.com/Anon-HP/Aletheia/internal/core"
	"github.com/Anon-HP/Aletheia/internal/node"
	"github.com/Anon-HP/Aletheia/internal/routing"
)

// Proof workflow errors.
var (
	// ErrNoNode: no node completed the request; every candidate was
	// unavailable (see isNodeFailure). The error also wraps each node's
	// failure, so api.IsTimeout and errors.Is work on it.
	ErrNoNode = errors.New("client: no node could serve the request")
	// ErrUnknownHeight: the proof is for a block above the client's own
	// tip. Synchronise headers first; a proof is never checked against a
	// header the client has not validated itself.
	ErrUnknownHeight = errors.New("client: proof is for a height the client has no header for")
	// ErrTxMismatch: the node's proof is for a transaction other than the
	// one the client asked about (DR-8 precondition c).
	ErrTxMismatch = errors.New("client: proof is for a different transaction")
	// ErrInvalidProof: the proof does not link the transaction to the
	// Merkle root in the client's own header at that height.
	ErrInvalidProof = errors.New("client: Merkle proof does not verify against the client's own header")
)

// LightClient submits transactions and verifies their inclusion without
// trusting any full node. It reaches the nodes only through a
// routing.Selector (DR-13: ring default, power of two choices, failover)
// and checks everything it receives itself:
//
//   - headers: by D2's consensus rules, from its own genesis (HeaderChain);
//   - proofs: txid computed locally (M-R7) and verified with D1's Merkle
//     verifier against the root in its own header (M-R8).
//
// The selector's probes run separately: pass api.Client.Probe to
// Selector.Run. Safe for concurrent use.
type LightClient struct {
	id      string // routing key: the ring maps it to the default node
	headers *HeaderChain
	sel     *routing.Selector
	api     *api.Client
}

// New returns a light client with routing key id (e.g. a client name),
// storing headers in headers and reaching nodes through sel and ac.
func New(id string, headers *HeaderChain, sel *routing.Selector, ac *api.Client) *LightClient {
	return &LightClient{id: id, headers: headers, sel: sel, api: ac}
}

// Headers returns the client's header store.
func (lc *LightClient) Headers() *HeaderChain { return lc.headers }

// isNodeFailure decides, for the error of one HTTP exchange with a node,
// whether the node failed (the selector counts a failure and tries the next
// candidate) or answered (the answer goes back to the caller and the
// exchange counts as successful communication).
//
// A failure is an exchange without a usable answer:
//   - no answer at all: connection refused or reset, a connect, request or
//     body timeout, a cancelled context, an answer larger than the client's
//     limit (anything that is neither an *api.APIError nor a decoding error);
//   - an answer with status 5xx: the node itself reports that it cannot
//     serve the request now (503 unavailable: node stopped; 500 internal;
//     502 sync_failed). For POST /tx, sending the same transaction to
//     another node is then safe: the transaction is identified by its
//     sender and sequence number, so it can be confirmed at most once.
//
// Everything else was answered and is not the node's availability problem:
//   - 4xx with an error code: an application-level rejection (400
//     duplicate, sender_pending, balance, seq, overflow, bad_request,
//     bad_locator; 404 not_found; 409 genesis_mismatch). The same request
//     sent elsewhere would be rejected the same way, or would duplicate a
//     transaction the node already judged;
//   - 4xx without a code (the router's plain 404/405): the node is reachable
//     but does not serve this request;
//   - a 2xx answer that does not decode (api.ErrMalformed). The node is up;
//     it answered badly, and that is reported to the caller.
func isNodeFailure(err error) bool {
	var ae *api.APIError
	if errors.As(err, &ae) {
		return ae.Status >= 500
	}
	return !errors.Is(err, api.ErrMalformed)
}

// do runs one request through the selector. call performs one HTTP
// exchange with the given node and returns its error.
//
// routing.Selector.Do counts every error its callback returns as a node
// failure and then tries the next node. So the callback returns call's
// error only when isNodeFailure says the node failed; an answer, including
// an application error, is returned to Do as nil (the exchange succeeded:
// the node's failure count is reset, its health flag is left alone, and no
// other node is tried) and kept here, to be returned to the caller
// unchanged. do returns the node that answered and its application error,
// if any.
//
// If no node answers, the error wraps ErrNoNode and every node's failure
// (with its type, which Do's own error would lose). If ctx ends, ctx's
// error is returned and no node is blamed (Do's rule).
func (lc *LightClient) do(ctx context.Context, call func(ctx context.Context, node string) error) (string, error) {
	var answer error
	var failures []error
	node, err := lc.sel.Do(ctx, lc.id, func(ctx context.Context, node string) error {
		err := call(ctx, node)
		if err != nil && isNodeFailure(err) {
			failures = append(failures, fmt.Errorf("%s: %w", node, err))
			return err
		}
		answer = err // nil, or the node's application-level answer
		return nil
	})
	switch {
	case err == nil:
		return node, answer
	case ctx.Err() != nil:
		return "", errors.Join(append(failures, ctx.Err())...)
	case len(failures) == 0:
		return "", err // e.g. routing.ErrEmpty: no nodes configured
	}
	return "", fmt.Errorf("%w: %w", ErrNoNode, errors.Join(failures...))
}

// Sync brings the header store up to date with the node the selector picks,
// in one locator round trip (HeaderChain.Sync describes the checks). If
// that node is unavailable the next candidate is asked; an answer that the
// client rejects (wrong genesis, invalid segment, bad fork point) is
// returned as an error and leaves the store unchanged.
func (lc *LightClient) Sync(ctx context.Context) (SyncResult, error) {
	var from string
	res, err := lc.headers.sync(ctx, func(ctx context.Context, loc []consensus.LocatorEntry) (fork uint64, hs []core.Header, err error) {
		from, err = lc.do(ctx, func(ctx context.Context, node string) error {
			var e error
			fork, hs, e = lc.api.HeadersAfter(ctx, node, loc)
			return e
		})
		return fork, hs, err
	})
	res.Node = from
	return res, err
}

// Submission is the outcome of Submit.
type Submission struct {
	TxID core.Hash // computed by the client itself (M-R7)
	Node string    // the node that answered: accepted, or rejected, the tx
}

// Submit sends tx through the selector (POST /tx).
//
// Send/report bookkeeping (DR-13): RecordSend before every attempt, so the
// node's estimate counts this request (k) until the next report; and
// RecordReport whenever the answer carries a valid X-Aletheia-Load header,
// rejected transactions included (the node measured that request too; the
// report replaces the old one and resets k).
//
// A rejection by the node (HTTP 400 duplicate, sender_pending, balance,
// seq, overflow, bad_request) is the node's answer: it is returned
// unchanged as the *api.APIError (errors.Is matches its sentinel, e.g.
// mempool.ErrSenderPending), Submission.Node names the node, the node's
// health is not touched, and the transaction is not sent to another node.
// Only an unavailable node (isNodeFailure) makes the selector try the next.
func (lc *LightClient) Submit(ctx context.Context, tx core.Tx) (Submission, error) {
	sub := Submission{TxID: core.TxID(tx)}
	var err error
	sub.Node, err = lc.do(ctx, func(ctx context.Context, node string) error {
		lc.sel.RecordSend(node)
		res, err := lc.api.SubmitTx(ctx, node, tx)
		if res.HasReport {
			lc.sel.RecordReport(node, res.Report)
		}
		return err
	})
	return sub, err
}

// Verified describes a transaction proven to be in the client's own chain.
type Verified struct {
	TxID   core.Hash // computed by the client (M-R7)
	Height uint64    // block height of the client's own header that holds it
	Node   string    // the node that supplied the proof
}

// Prove fetches the inclusion proof of tx through the selector
// (GET /proof/{txid}) and verifies it locally with VerifyProof. The txid
// in the request is computed by the client. A 404 (not_found: unknown or
// still pending at that node) is the node's answer and is returned as is
// (errors.Is(err, core.ErrNotFound)); only an unavailable node is failed
// over. Synchronise headers first: a proof above the client's tip fails
// with ErrUnknownHeight.
func (lc *LightClient) Prove(ctx context.Context, tx core.Tx) (Verified, error) {
	v := Verified{TxID: core.TxID(tx)}
	var pr node.ProofResult
	var err error
	v.Node, err = lc.do(ctx, func(ctx context.Context, n string) error {
		var e error
		pr, e = lc.api.Proof(ctx, n, v.TxID)
		return e
	})
	if err != nil {
		return v, err
	}
	if err := VerifyProof(lc.headers, tx, pr); err != nil {
		return v, err
	}
	v.Height = pr.Height
	return v, nil
}

// VerifyProof checks, entirely locally, that pr proves tx is in the block at
// pr.Height of the client's own header chain hc:
//
//  1. pr.Tx must equal tx in every field (DR-8 precondition c: a proof only
//     shows that some 88 bytes are in a block, so the client checks they
//     are the transaction it means);
//  2. the leaf is core.TxID(tx), computed here from the 88-byte canonical
//     encoding (M-R7): no txid or leaf hash is taken from the node, so an
//     internal Merkle node can never pass as a transaction;
//  3. the client must hold its own validated header at pr.Height, above
//     genesis (genesis has no transactions, M-R5);
//  4. core.MerkleVerify(root of that header, leaf, pr.Proof) must accept
//     (M-R8: the root is the client's own; the proof answer carries none
//     and nothing a node sends is used as a root). This is D1's single
//     verifier, O(log n) hashes for a block of n transactions.
func VerifyProof(hc *HeaderChain, tx core.Tx, pr node.ProofResult) error {
	if pr.Tx != tx {
		return ErrTxMismatch
	}
	leaf := core.TxID(tx)
	if pr.Height == 0 {
		return fmt.Errorf("%w: height 0 is genesis, which holds no transactions", ErrInvalidProof)
	}
	h, ok := hc.Header(pr.Height)
	if !ok {
		return fmt.Errorf("%w: height %d, own tip %d", ErrUnknownHeight, pr.Height, hc.Height())
	}
	if !core.MerkleVerify(h.MerkleRoot, leaf, pr.Proof) {
		return fmt.Errorf("%w: height %d", ErrInvalidProof, pr.Height)
	}
	return nil
}
