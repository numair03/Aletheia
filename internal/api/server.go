package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"

	"github.com/Anon-HP/Aletheia/internal/consensus"
	"github.com/Anon-HP/Aletheia/internal/core"
	"github.com/Anon-HP/Aletheia/internal/mempool"
	"github.com/Anon-HP/Aletheia/internal/node"
	"github.com/Anon-HP/Aletheia/internal/routing"
)

// Request body limits. A transaction is a few hundred bytes of JSON; a
// block holds at most MaxTxPerBlock (100) of them.
const (
	maxTxBody    = 16 << 10
	maxBlockBody = 4 << 20
)

// ErrFetch wraps every error of PeerTransport.FetchBlocks, so the server can
// tell "the sender's chain could not be fetched" (502) from other failures.
var ErrFetch = errors.New("api: fetching blocks from peer failed")

// Backend is what the handlers need from a full node; *node.Node implements
// it. Tests substitute a wrapper to script request timing.
type Backend interface {
	SubmitTx(tx core.Tx) (core.Hash, error)
	Tip() (uint64, core.Header)
	MempoolSize() int
	Headers(from uint64) []core.Header
	Blocks(from uint64) []core.Block
	HeadersAfter(loc []consensus.LocatorEntry) (uint64, []core.Header, error)
	BlocksAfter(loc []consensus.LocatorEntry) (uint64, []core.Block, error)
	Proof(txid core.Hash) (node.ProofResult, error)
	ReceiveBlock(b core.Block, fromPeer string) error
}

// Server serves a full node over HTTP:
//
//	POST /tx            submit a transaction (measured by the LoadMeter)
//	GET  /tip           height, header, mempool size; load report header
//	GET  /headers       ?from=<h> or ?locator=<h>:<hex>,…
//	GET  /blocks        ?from=<h> or ?locator=<h>:<hex>,…
//	GET  /proof/{txid}  Merkle inclusion proof, no root
//	POST /block         a block from a peer (relay)
//
// Every error answer is an ErrorResponse with a status from the table in
// dto.go. Every POST /tx answer, error or not, and every GET /tip answer
// carry the X-Aletheia-Load header once the meter has a sample (DR-13).
type Server struct {
	b     Backend
	meter *routing.LoadMeter
	mux   *http.ServeMux
}

// NewServer creates the handlers for b. meter measures POST /tx; nil creates
// one on the real clock.
func NewServer(b Backend, meter *routing.LoadMeter) *Server {
	if meter == nil {
		meter = routing.NewLoadMeter(nil)
	}
	s := &Server{b: b, meter: meter, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /tx", s.measured(s.handleTx))
	s.mux.HandleFunc("GET /tip", s.handleTip)
	s.mux.HandleFunc("GET /headers", s.handleHeaders)
	s.mux.HandleFunc("GET /blocks", s.handleBlocks)
	s.mux.HandleFunc("GET /proof/{txid}", s.handleProof)
	s.mux.HandleFunc("POST /block", s.handleBlock)
	return s
}

// Meter returns the server's POST /tx load meter.
func (s *Server) Meter() *routing.LoadMeter { return s.meter }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// ---- Load measurement (DR-13) ----

// bufferedResponse holds a handler's answer until the measurement is done.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedResponse) WriteHeader(status int)      { b.status = status }

// measured wraps the POST /tx handler with the LoadMeter. The sojourn runs
// from entry (before the body is read) until the handler has produced its
// complete answer; the answer is then sent with the report taken after this
// request's own sample. So the report counts only the requests still in
// flight (n excludes this one, which is finished) and already includes this
// request's service time. The transfer of the answer to the client is
// network time and is not part of the node's queueing delay.
func (s *Server) measured(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		done := s.meter.Start()
		defer done() // idempotent; covers a panicking handler
		rec := &bufferedResponse{header: make(http.Header), status: http.StatusOK}
		h(rec, r)
		done()
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		s.setLoadHeader(w)
		w.WriteHeader(rec.status)
		w.Write(rec.body.Bytes())
	}
}

func (s *Server) setLoadHeader(w http.ResponseWriter) {
	if rep, ok := s.meter.Report(); ok {
		w.Header().Set(routing.LoadHeader, rep.String())
	}
}

// ---- Responses ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string, err error) {
	writeJSON(w, status, ErrorResponse{Code: code, Error: err.Error()})
}

// classify maps an error from the node to a status and an error code.
func classify(err error) (int, string) {
	switch {
	case errors.Is(err, ErrMalformed):
		return http.StatusBadRequest, CodeBadRequest
	case errors.Is(err, mempool.ErrDuplicate):
		return http.StatusBadRequest, CodeDuplicate
	case errors.Is(err, mempool.ErrSenderPending):
		return http.StatusBadRequest, CodeSenderPending
	case errors.Is(err, consensus.ErrBadLocator):
		return http.StatusBadRequest, CodeBadLocator
	case errors.Is(err, consensus.ErrGenesisMismatch):
		return http.StatusConflict, CodeGenesisMismatch
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound, CodeNotFound
	case errors.Is(err, node.ErrStopped), errors.Is(err, mempool.ErrClosed), errors.Is(err, core.ErrClosed):
		return http.StatusServiceUnavailable, CodeUnavailable
	}
	return http.StatusInternalServerError, CodeInternal
}

// classifyTx maps a SubmitTx error: the ledger rule errors are rejections of
// the transaction.
func classifyTx(err error) (int, string) {
	switch {
	case errors.Is(err, core.ErrBalance):
		return http.StatusBadRequest, CodeBalance
	case errors.Is(err, core.ErrSeq):
		return http.StatusBadRequest, CodeSeq
	case errors.Is(err, core.ErrOverflow):
		return http.StatusBadRequest, CodeOverflow
	}
	return classify(err)
}

// classifyBlock maps a ReceiveBlock error. Validation failures of the block
// or of the sender's chain are the sender's fault (400); a failed fetch of
// the sender's chain is 502.
func classifyBlock(err error) (int, string) {
	switch {
	case errors.Is(err, node.ErrPeerChain),
		errors.Is(err, consensus.ErrLinkage), errors.Is(err, consensus.ErrDifficulty), errors.Is(err, consensus.ErrWork),
		errors.Is(err, core.ErrLinkage), errors.Is(err, core.ErrMerkle), errors.Is(err, core.ErrTxSet),
		errors.Is(err, core.ErrBalance), errors.Is(err, core.ErrSeq), errors.Is(err, core.ErrOverflow):
		return http.StatusBadRequest, CodeInvalidBlock
	case errors.Is(err, ErrFetch), errors.Is(err, node.ErrNoPeer):
		return http.StatusBadGateway, CodeSyncFailed
	}
	return classify(err)
}

// decodeBody decodes exactly one JSON value with no unknown fields and
// nothing after it; errors wrap ErrMalformed.
func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("%w: data after the JSON value", ErrMalformed)
	}
	return nil
}

// rangeQuery reads exactly one of ?from=<h> and ?locator=<…>.
func rangeQuery(r *http.Request) (from uint64, loc []consensus.LocatorEntry, err error) {
	q := r.URL.Query()
	fs, hasFrom := q["from"]
	ls, hasLoc := q["locator"]
	switch {
	case hasFrom == hasLoc:
		return 0, nil, fmt.Errorf("%w: give exactly one of from and locator", ErrMalformed)
	case hasFrom:
		if len(fs) != 1 {
			return 0, nil, fmt.Errorf("%w: from given more than once", ErrMalformed)
		}
		from, err = strconv.ParseUint(fs[0], 10, 64)
		if err != nil {
			return 0, nil, fmt.Errorf("%w: from must be a height", ErrMalformed)
		}
		return from, nil, nil
	default:
		if len(ls) != 1 {
			return 0, nil, fmt.Errorf("%w: locator given more than once", consensus.ErrBadLocator)
		}
		loc, err = ParseLocator(ls[0])
		return 0, loc, err
	}
}

// ---- Handlers ----

func (s *Server) handleTx(w http.ResponseWriter, r *http.Request) {
	var j TxJSON
	if err := decodeBody(w, r, maxTxBody, &j); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err)
		return
	}
	tx, err := TxFromJSON(j)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err)
		return
	}
	id, err := s.b.SubmitTx(tx)
	if err != nil {
		st, code := classifyTx(err)
		writeError(w, st, code, err)
		return
	}
	writeJSON(w, http.StatusOK, SubmitResponse{TxID: id.String()})
}

func (s *Server) handleTip(w http.ResponseWriter, r *http.Request) {
	h, hd := s.b.Tip()
	s.setLoadHeader(w)
	writeJSON(w, http.StatusOK, TipResponse{Height: h, Header: HeaderToJSON(hd), MempoolSize: s.b.MempoolSize()})
}

func (s *Server) handleHeaders(w http.ResponseWriter, r *http.Request) {
	from, loc, err := rangeQuery(r)
	if err != nil {
		st, code := classify(err)
		writeError(w, st, code, err)
		return
	}
	if loc == nil {
		writeJSON(w, http.StatusOK, HeadersFromResponse{From: from, Headers: HeadersToJSON(s.b.Headers(from))})
		return
	}
	fork, hs, err := s.b.HeadersAfter(loc)
	if err != nil {
		st, code := classify(err)
		writeError(w, st, code, err)
		return
	}
	writeJSON(w, http.StatusOK, HeadersAfterResponse{Fork: fork, Headers: HeadersToJSON(hs)})
}

func (s *Server) handleBlocks(w http.ResponseWriter, r *http.Request) {
	from, loc, err := rangeQuery(r)
	if err != nil {
		st, code := classify(err)
		writeError(w, st, code, err)
		return
	}
	if loc == nil {
		writeJSON(w, http.StatusOK, BlocksFromResponse{From: from, Blocks: BlocksToJSON(s.b.Blocks(from))})
		return
	}
	fork, bs, err := s.b.BlocksAfter(loc)
	if err != nil {
		st, code := classify(err)
		writeError(w, st, code, err)
		return
	}
	writeJSON(w, http.StatusOK, BlocksAfterResponse{Fork: fork, Blocks: BlocksToJSON(bs)})
}

func (s *Server) handleProof(w http.ResponseWriter, r *http.Request) {
	txid, err := parseHash("txid", r.PathValue("txid"))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err)
		return
	}
	res, err := s.b.Proof(txid)
	if err != nil {
		st, code := classify(err)
		writeError(w, st, code, err)
		return
	}
	writeJSON(w, http.StatusOK, ProofToJSON(res))
}

func (s *Server) handleBlock(w http.ResponseWriter, r *http.Request) {
	var req BlockRequest
	if err := decodeBody(w, r, maxBlockBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err)
		return
	}
	if _, _, err := net.SplitHostPort(req.From); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, fmt.Errorf("%w: from must be host:port: %v", ErrMalformed, err))
		return
	}
	b, err := BlockFromJSON(req.Block)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err)
		return
	}
	switch err := s.b.ReceiveBlock(b, req.From); {
	case err == nil:
		writeJSON(w, http.StatusOK, BlockResponse{Result: ResultAccepted})
	case errors.Is(err, node.ErrStale):
		writeJSON(w, http.StatusOK, BlockResponse{Result: ResultStale})
	default:
		st, code := classifyBlock(err)
		writeError(w, st, code, err)
	}
}
