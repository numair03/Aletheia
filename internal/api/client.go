package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Anon-HP/Aletheia/internal/consensus"
	"github.com/Anon-HP/Aletheia/internal/core"
	"github.com/Anon-HP/Aletheia/internal/node"
	"github.com/Anon-HP/Aletheia/internal/routing"
)

// maxResponseBody bounds what the client reads from one answer (a whole
// chain of full blocks fits easily at demonstration scale).
const maxResponseBody = 64 << 20

// APIError is an error answer from a node: the HTTP status and the
// ErrorResponse it carried. For codes that stand for a sentinel error
// (duplicate, sender_pending, balance, seq, overflow, bad_locator,
// not_found, genesis_mismatch, bad_request), errors.Is matches that
// sentinel, e.g. errors.Is(err, mempool.ErrSenderPending).
type APIError struct {
	Status  int
	Code    string // empty if the body was not an ErrorResponse
	Message string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("api: HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("api: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

// Unwrap returns the sentinel error of the code, if any.
func (e *APIError) Unwrap() error { return codeErr(e.Code) }

// Client calls the HTTP API of full nodes. Nodes are addressed as host:port.
// The timeouts are fixed (DR-13): connect routing.ConnectTimeout, and a
// whole request (connect, send, read the answer) routing.RequestTimeout.
// A caller's context may shorten them (Probe uses routing.ProbeTimeout).
// Safe for concurrent use.
type Client struct {
	hc *http.Client
}

// NewClient creates a client with the fixed timeouts.
func NewClient() *Client {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: routing.ConnectTimeout}).DialContext,
		TLSHandshakeTimeout:   routing.ConnectTimeout,
		ResponseHeaderTimeout: routing.RequestTimeout,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &Client{hc: &http.Client{Transport: tr, Timeout: routing.RequestTimeout}}
}

// CloseIdleConnections closes idle keep-alive connections.
func (c *Client) CloseIdleConnections() { c.hc.CloseIdleConnections() }

// do sends one request and decodes a 2xx JSON answer into out. It returns
// the answer's header (also for error answers, so the load report of a
// failed POST /tx is not lost). Transport errors, timeouts and non-2xx
// answers are errors; a non-2xx answer is an *APIError.
func (c *Client) do(ctx context.Context, method, nodeAddr, path string, body, out any) (http.Header, error) {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+nodeAddr+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return resp.Header, err
	}
	if len(data) > maxResponseBody {
		return resp.Header, fmt.Errorf("api: answer from %s exceeds %d bytes", nodeAddr, maxResponseBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var er ErrorResponse
		if json.Unmarshal(data, &er) == nil && er.Code != "" {
			return resp.Header, &APIError{Status: resp.StatusCode, Code: er.Code, Message: er.Error}
		}
		return resp.Header, &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return resp.Header, fmt.Errorf("%w: answer from %s: %v", ErrMalformed, nodeAddr, err)
	}
	return resp.Header, nil
}

// loadReport reads the X-Aletheia-Load header. A missing or malformed
// header gives no report (the node sends none before its first sample).
func loadReport(h http.Header) (routing.LoadReport, bool) {
	v := h.Get(routing.LoadHeader)
	if v == "" {
		return routing.LoadReport{}, false
	}
	rep, err := routing.ParseLoadReport(v)
	return rep, err == nil
}

// SubmitResult is the outcome of POST /tx. Report is filled whenever the
// node answered with a load header, also when the transaction was rejected,
// so the caller can pass it to Selector.RecordReport.
type SubmitResult struct {
	TxID      core.Hash
	Report    routing.LoadReport
	HasReport bool
}

// SubmitTx sends tx to nodeAddr. The txid in the answer must equal the one
// computed locally (by the C++ core), otherwise the answer is malformed.
func (c *Client) SubmitTx(ctx context.Context, nodeAddr string, tx core.Tx) (SubmitResult, error) {
	var resp SubmitResponse
	h, err := c.do(ctx, http.MethodPost, nodeAddr, "/tx", TxToJSON(tx), &resp)
	var res SubmitResult
	if h != nil {
		res.Report, res.HasReport = loadReport(h)
	}
	if err != nil {
		return res, err
	}
	id, err := parseHash("txid", resp.TxID)
	if err != nil {
		return res, err
	}
	if id != core.TxID(tx) {
		return res, fmt.Errorf("%w: node returned txid %s for a transaction with txid %s", ErrMalformed, id, core.TxID(tx))
	}
	res.TxID = id
	return res, nil
}

// TipInfo is the answer of GET /tip.
type TipInfo struct {
	Height      uint64
	Header      core.Header
	MempoolSize int // informational only; never used for routing
	Report      routing.LoadReport
	HasReport   bool
}

// Tip asks nodeAddr for its tip.
func (c *Client) Tip(ctx context.Context, nodeAddr string) (TipInfo, error) {
	var resp TipResponse
	h, err := c.do(ctx, http.MethodGet, nodeAddr, "/tip", nil, &resp)
	if err != nil {
		return TipInfo{}, err
	}
	hd, err := HeaderFromJSON(resp.Header)
	if err != nil {
		return TipInfo{}, err
	}
	info := TipInfo{Height: resp.Height, Header: hd, MempoolSize: resp.MempoolSize}
	info.Report, info.HasReport = loadReport(h)
	return info, nil
}

// Probe is the selector's probe (routing.ProbeFunc): one GET /tip, limited
// to routing.ProbeTimeout, returning the round-trip time and the node's
// load report.
func (c *Client) Probe(ctx context.Context, nodeAddr string) (routing.ProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, routing.ProbeTimeout)
	defer cancel()
	start := time.Now()
	info, err := c.Tip(ctx, nodeAddr)
	if err != nil {
		return routing.ProbeResult{}, err
	}
	return routing.ProbeResult{RTT: time.Since(start), Report: info.Report, HasReport: info.HasReport}, nil
}

// Headers returns nodeAddr's headers from height from up to its tip.
func (c *Client) Headers(ctx context.Context, nodeAddr string, from uint64) ([]core.Header, error) {
	var resp HeadersFromResponse
	if _, err := c.do(ctx, http.MethodGet, nodeAddr, "/headers?from="+strconv.FormatUint(from, 10), nil, &resp); err != nil {
		return nil, err
	}
	if resp.From != from {
		return nil, fmt.Errorf("%w: asked for headers from %d, got from %d", ErrMalformed, from, resp.From)
	}
	return HeadersFromJSON(resp.Headers)
}

// HeadersAfter sends loc to nodeAddr and returns its fork point and its
// headers after it (DR-14). Checking them is the caller's job.
func (c *Client) HeadersAfter(ctx context.Context, nodeAddr string, loc []consensus.LocatorEntry) (uint64, []core.Header, error) {
	var resp HeadersAfterResponse
	if _, err := c.do(ctx, http.MethodGet, nodeAddr, "/headers?locator="+FormatLocator(loc), nil, &resp); err != nil {
		return 0, nil, err
	}
	hs, err := HeadersFromJSON(resp.Headers)
	return resp.Fork, hs, err
}

// Blocks returns nodeAddr's blocks from height from up to its tip.
func (c *Client) Blocks(ctx context.Context, nodeAddr string, from uint64) ([]core.Block, error) {
	var resp BlocksFromResponse
	if _, err := c.do(ctx, http.MethodGet, nodeAddr, "/blocks?from="+strconv.FormatUint(from, 10), nil, &resp); err != nil {
		return nil, err
	}
	if resp.From != from {
		return nil, fmt.Errorf("%w: asked for blocks from %d, got from %d", ErrMalformed, from, resp.From)
	}
	return BlocksFromJSON(resp.Blocks)
}

// BlocksAfter sends loc to nodeAddr and returns its fork point and its
// blocks after it (DR-14).
func (c *Client) BlocksAfter(ctx context.Context, nodeAddr string, loc []consensus.LocatorEntry) (uint64, []core.Block, error) {
	var resp BlocksAfterResponse
	if _, err := c.do(ctx, http.MethodGet, nodeAddr, "/blocks?locator="+FormatLocator(loc), nil, &resp); err != nil {
		return 0, nil, err
	}
	bs, err := BlocksFromJSON(resp.Blocks)
	return resp.Fork, bs, err
}

// Proof asks nodeAddr for the inclusion proof of txid. The answer is only
// decoded: the light client checks it against its own header (M-R7, M-R8).
func (c *Client) Proof(ctx context.Context, nodeAddr string, txid core.Hash) (node.ProofResult, error) {
	var resp ProofResponse
	if _, err := c.do(ctx, http.MethodGet, nodeAddr, "/proof/"+txid.String(), nil, &resp); err != nil {
		return node.ProofResult{}, err
	}
	return ProofFromJSON(resp)
}

// PostBlock sends b to nodeAddr; from is this node's listen address, from
// which the receiver fetches missing blocks. It returns ResultAccepted or
// ResultStale.
func (c *Client) PostBlock(ctx context.Context, nodeAddr string, b core.Block, from string) (string, error) {
	var resp BlockResponse
	if _, err := c.do(ctx, http.MethodPost, nodeAddr, "/block", BlockRequest{Block: BlockToJSON(b), From: from}, &resp); err != nil {
		return "", err
	}
	if resp.Result != ResultAccepted && resp.Result != ResultStale {
		return "", fmt.Errorf("%w: block result %q", ErrMalformed, resp.Result)
	}
	return resp.Result, nil
}

// IsTimeout reports whether err is a timeout (connect, request or probe).
func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
