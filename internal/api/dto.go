// Package api is the HTTP/JSON interface between full nodes, and between
// full nodes and light clients: the server handlers (server.go), the client
// (client.go) and block relay between peers (peers.go).
//
// The JSON wire format in this file is a transport format only. It is never
// hashed: every txid and block hash is recomputed by the C++ core from the
// decoded fields (DECISIONS DR-10 §5). Hashes travel as 64 lowercase hex
// digits; numbers as JSON integers (uint64 values are exact in Go's
// encoding/json).
package api

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Anon-HP/Aletheia/internal/consensus"
	"github.com/Anon-HP/Aletheia/internal/core"
	"github.com/Anon-HP/Aletheia/internal/mempool"
	"github.com/Anon-HP/Aletheia/internal/node"
)

// ErrMalformed: a request or response field could not be decoded (bad JSON,
// bad hex, bad proof side, bad query parameter).
var ErrMalformed = errors.New("api: malformed message")

// ---- Wire types ----

// TxJSON is a transaction on the wire.
type TxJSON struct {
	Sender    string `json:"sender"`
	Recipient string `json:"recipient"`
	Amount    uint64 `json:"amount"`
	Fee       uint64 `json:"fee"`
	Seq       uint64 `json:"seq"`
}

// HeaderJSON is a block header on the wire. It carries no hash.
type HeaderJSON struct {
	Version    uint32 `json:"version"`
	Prev       string `json:"prev"`
	MerkleRoot string `json:"merkle_root"`
	Timestamp  int64  `json:"timestamp"`
	Difficulty uint32 `json:"difficulty"`
	Nonce      uint64 `json:"nonce"`
}

// BlockJSON is a block on the wire.
type BlockJSON struct {
	Header HeaderJSON `json:"header"`
	Txs    []TxJSON   `json:"txs"`
}

// ProofStepJSON is one Merkle proof step: the sibling and the side it sits
// on, "left" or "right" (core.Left, core.Right).
type ProofStepJSON struct {
	Sibling string `json:"sibling"`
	Side    string `json:"side"`
}

// ---- Request and response bodies ----

// SubmitResponse answers POST /tx: the txid the node computed.
type SubmitResponse struct {
	TxID string `json:"txid"`
}

// TipResponse answers GET /tip. MempoolSize is informational only and is
// never used for routing (CD-29 item 2); the load report travels in the
// X-Aletheia-Load header.
type TipResponse struct {
	Height      uint64     `json:"height"`
	Header      HeaderJSON `json:"header"`
	MempoolSize int        `json:"mempool_size"`
}

// HeadersFromResponse answers GET /headers?from=h: headers h..tip.
type HeadersFromResponse struct {
	From    uint64       `json:"from"`
	Headers []HeaderJSON `json:"headers"`
}

// HeadersAfterResponse answers GET /headers?locator=…: the fork point and
// the headers fork+1..tip (DR-14).
type HeadersAfterResponse struct {
	Fork    uint64       `json:"fork"`
	Headers []HeaderJSON `json:"headers"`
}

// BlocksFromResponse answers GET /blocks?from=h.
type BlocksFromResponse struct {
	From   uint64      `json:"from"`
	Blocks []BlockJSON `json:"blocks"`
}

// BlocksAfterResponse answers GET /blocks?locator=…
type BlocksAfterResponse struct {
	Fork   uint64      `json:"fork"`
	Blocks []BlockJSON `json:"blocks"`
}

// ProofResponse answers GET /proof/{txid}. It carries no Merkle root: the
// client verifies against the root in its own header at Height (M-R8).
type ProofResponse struct {
	Height uint64          `json:"height"`
	Tx     TxJSON          `json:"tx"`
	Proof  []ProofStepJSON `json:"proof"`
}

// BlockRequest is the body of POST /block. From is the sender's listen
// address (host:port): if the block does not extend the receiver's tip, the
// receiver fetches the missing blocks from there.
type BlockRequest struct {
	Block BlockJSON `json:"block"`
	From  string    `json:"from"`
}

// BlockResponse answers POST /block: "accepted" (the chain changed) or
// "stale" (the block was already known or does not lead to a longer chain).
type BlockResponse struct {
	Result string `json:"result"`
}

// Results of POST /block.
const (
	ResultAccepted = "accepted"
	ResultStale    = "stale"
)

// ErrorResponse is the body of every error answer of the handlers.
type ErrorResponse struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// ---- Error codes ----

// Error codes in ErrorResponse.Code.
const (
	CodeBadRequest      = "bad_request"      // 400: malformed JSON, hex or query
	CodeDuplicate       = "duplicate"        // 400: transaction already pending
	CodeSenderPending   = "sender_pending"   // 400: sender has a pending transaction (O-4)
	CodeBalance         = "balance"          // 400: sender cannot pay amount + fee
	CodeSeq             = "seq"              // 400: wrong sequence number
	CodeOverflow        = "overflow"         // 400: amount overflow
	CodeInvalidBlock    = "invalid_block"    // 400: block or peer chain failed validation
	CodeBadLocator      = "bad_locator"      // 400: malformed locator (DR-14)
	CodeNotFound        = "not_found"        // 404: transaction not in the chain
	CodeGenesisMismatch = "genesis_mismatch" // 409: the requester's genesis differs
	CodeSyncFailed      = "sync_failed"      // 502: fetching the sender's chain failed
	CodeUnavailable     = "unavailable"      // 503: node stopped
	CodeInternal        = "internal"         // 500: anything else
)

// codeErr returns the sentinel error a code stands for, so that a client can
// use errors.Is across the network (nil for codes without one).
func codeErr(code string) error {
	switch code {
	case CodeDuplicate:
		return mempool.ErrDuplicate
	case CodeSenderPending:
		return mempool.ErrSenderPending
	case CodeBalance:
		return core.ErrBalance
	case CodeSeq:
		return core.ErrSeq
	case CodeOverflow:
		return core.ErrOverflow
	case CodeBadLocator:
		return consensus.ErrBadLocator
	case CodeNotFound:
		return core.ErrNotFound
	case CodeGenesisMismatch:
		return consensus.ErrGenesisMismatch
	case CodeBadRequest:
		return ErrMalformed
	}
	return nil
}

// ---- Conversions ----

func parseHash(field, s string) (core.Hash, error) {
	h, err := core.ParseHash(s)
	if err != nil {
		return core.Hash{}, fmt.Errorf("%w: %s: %v", ErrMalformed, field, err)
	}
	return h, nil
}

// TxToJSON converts a transaction to its wire form.
func TxToJSON(t core.Tx) TxJSON {
	return TxJSON{Sender: t.Sender.String(), Recipient: t.Recipient.String(), Amount: t.Amount, Fee: t.Fee, Seq: t.Seq}
}

// TxFromJSON decodes a wire transaction; errors wrap ErrMalformed.
func TxFromJSON(j TxJSON) (core.Tx, error) {
	s, err := parseHash("sender", j.Sender)
	if err != nil {
		return core.Tx{}, err
	}
	r, err := parseHash("recipient", j.Recipient)
	if err != nil {
		return core.Tx{}, err
	}
	return core.Tx{Sender: s, Recipient: r, Amount: j.Amount, Fee: j.Fee, Seq: j.Seq}, nil
}

// HeaderToJSON converts a header to its wire form.
func HeaderToJSON(h core.Header) HeaderJSON {
	return HeaderJSON{Version: h.Version, Prev: h.Prev.String(), MerkleRoot: h.MerkleRoot.String(),
		Timestamp: h.Timestamp, Difficulty: h.Difficulty, Nonce: h.Nonce}
}

// HeaderFromJSON decodes a wire header; errors wrap ErrMalformed.
func HeaderFromJSON(j HeaderJSON) (core.Header, error) {
	prev, err := parseHash("prev", j.Prev)
	if err != nil {
		return core.Header{}, err
	}
	root, err := parseHash("merkle_root", j.MerkleRoot)
	if err != nil {
		return core.Header{}, err
	}
	return core.Header{Version: j.Version, Prev: prev, MerkleRoot: root,
		Timestamp: j.Timestamp, Difficulty: j.Difficulty, Nonce: j.Nonce}, nil
}

// BlockToJSON converts a block to its wire form (txs is never null).
func BlockToJSON(b core.Block) BlockJSON {
	txs := make([]TxJSON, len(b.Txs))
	for i, t := range b.Txs {
		txs[i] = TxToJSON(t)
	}
	return BlockJSON{Header: HeaderToJSON(b.Header), Txs: txs}
}

// BlockFromJSON decodes a wire block; errors wrap ErrMalformed.
func BlockFromJSON(j BlockJSON) (core.Block, error) {
	h, err := HeaderFromJSON(j.Header)
	if err != nil {
		return core.Block{}, err
	}
	txs := make([]core.Tx, len(j.Txs))
	for i, tj := range j.Txs {
		if txs[i], err = TxFromJSON(tj); err != nil {
			return core.Block{}, fmt.Errorf("tx %d: %w", i, err)
		}
	}
	return core.Block{Header: h, Txs: txs}, nil
}

// HeadersToJSON converts headers (never null).
func HeadersToJSON(hs []core.Header) []HeaderJSON {
	out := make([]HeaderJSON, len(hs))
	for i, h := range hs {
		out[i] = HeaderToJSON(h)
	}
	return out
}

// HeadersFromJSON decodes headers; errors wrap ErrMalformed.
func HeadersFromJSON(js []HeaderJSON) ([]core.Header, error) {
	out := make([]core.Header, len(js))
	for i, j := range js {
		var err error
		if out[i], err = HeaderFromJSON(j); err != nil {
			return nil, fmt.Errorf("header %d: %w", i, err)
		}
	}
	return out, nil
}

// BlocksToJSON converts blocks (never null).
func BlocksToJSON(bs []core.Block) []BlockJSON {
	out := make([]BlockJSON, len(bs))
	for i, b := range bs {
		out[i] = BlockToJSON(b)
	}
	return out
}

// BlocksFromJSON decodes blocks; errors wrap ErrMalformed.
func BlocksFromJSON(js []BlockJSON) ([]core.Block, error) {
	out := make([]core.Block, len(js))
	for i, j := range js {
		var err error
		if out[i], err = BlockFromJSON(j); err != nil {
			return nil, fmt.Errorf("block %d: %w", i, err)
		}
	}
	return out, nil
}

// ProofToJSON converts a proof result to its wire form.
func ProofToJSON(r node.ProofResult) ProofResponse {
	steps := make([]ProofStepJSON, len(r.Proof))
	for i, s := range r.Proof {
		side := "left"
		if s.Side == core.Right {
			side = "right"
		}
		steps[i] = ProofStepJSON{Sibling: s.Sibling.String(), Side: side}
	}
	return ProofResponse{Height: r.Height, Tx: TxToJSON(r.Tx), Proof: steps}
}

// ProofFromJSON decodes a proof answer; errors wrap ErrMalformed. A proof
// longer than core.MaxProofSteps is rejected here; whether it is valid is
// for core.MerkleVerify to decide, against the client's own header.
func ProofFromJSON(j ProofResponse) (node.ProofResult, error) {
	tx, err := TxFromJSON(j.Tx)
	if err != nil {
		return node.ProofResult{}, err
	}
	if len(j.Proof) > core.MaxProofSteps {
		return node.ProofResult{}, fmt.Errorf("%w: proof has %d steps, at most %d", ErrMalformed, len(j.Proof), core.MaxProofSteps)
	}
	proof := make(core.Proof, len(j.Proof))
	for i, s := range j.Proof {
		sib, err := parseHash(fmt.Sprintf("proof step %d", i), s.Sibling)
		if err != nil {
			return node.ProofResult{}, err
		}
		var side core.Side
		switch s.Side {
		case "left":
			side = core.Left
		case "right":
			side = core.Right
		default:
			return node.ProofResult{}, fmt.Errorf("%w: proof step %d: side %q", ErrMalformed, i, s.Side)
		}
		proof[i] = core.ProofStep{Sibling: sib, Side: side}
	}
	return node.ProofResult{Height: j.Height, Tx: tx, Proof: proof}, nil
}

// ---- Locator query parameter (DR-14) ----

// FormatLocator renders a locator as the query value
// "<height>:<hex>,<height>:<hex>,…".
func FormatLocator(loc []consensus.LocatorEntry) string {
	var b strings.Builder
	for i, e := range loc {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(e.Height, 10))
		b.WriteByte(':')
		b.WriteString(e.Hash.String())
	}
	return b.String()
}

// ParseLocator parses a locator query value. It checks only the syntax and
// the entry count (at most consensus.MaxLocatorEntries, counted before
// anything is parsed); ordering and the genesis entry are checked by
// consensus.ForkPoint. Errors wrap consensus.ErrBadLocator.
func ParseLocator(s string) ([]consensus.LocatorEntry, error) {
	if s == "" {
		return nil, fmt.Errorf("%w: empty", consensus.ErrBadLocator)
	}
	if n := strings.Count(s, ",") + 1; n > consensus.MaxLocatorEntries {
		return nil, fmt.Errorf("%w: %d entries, at most %d allowed", consensus.ErrBadLocator, n, consensus.MaxLocatorEntries)
	}
	parts := strings.Split(s, ",")
	loc := make([]consensus.LocatorEntry, len(parts))
	for i, p := range parts {
		hs, xs, ok := strings.Cut(p, ":")
		if !ok {
			return nil, fmt.Errorf("%w: entry %d: want <height>:<hash>", consensus.ErrBadLocator, i)
		}
		h, err := strconv.ParseUint(hs, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d: bad height", consensus.ErrBadLocator, i)
		}
		x, err := core.ParseHash(xs)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d: %v", consensus.ErrBadLocator, i, err)
		}
		loc[i] = consensus.LocatorEntry{Height: h, Hash: x}
	}
	return loc, nil
}
