package routing

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LoadHeader is the HTTP response header that carries a node's load report,
// formatted by LoadReport.String: "d=<µs>,n=<int>". Nodes send it on every
// POST /tx response and on GET /tip.
const LoadHeader = "X-Aletheia-Load"

// EWMAAlpha is the weight of the newest sample in every moving average of the
// routing package: the server's service-time estimate and the client's
// round-trip time. avg ← avg + α·(sample − avg).
const EWMAAlpha = 0.2

// LoadReport is a node's report of its POST /tx load.
//
// Delay is D = (n+1)·ŝ: the expected time a new submission arriving now would
// spend in the node (queueing plus its own service). InFlight is n, the
// POST /tx requests in the node at the time of the report. A report carries no
// timestamp, because the clocks of different machines disagree; the client
// ages it by its own receive time.
type LoadReport struct {
	Delay    time.Duration
	InFlight int
}

// String formats r for LoadHeader, with the delay in whole microseconds.
func (r LoadReport) String() string {
	return "d=" + strconv.FormatInt(r.Delay.Microseconds(), 10) + ",n=" + strconv.Itoa(r.InFlight)
}

// ErrBadLoadReport is returned by ParseLoadReport for a malformed header.
var ErrBadLoadReport = errors.New("routing: malformed load report")

// ParseLoadReport parses a LoadHeader value of the exact form "d=<µs>,n=<int>"
// with non-negative decimal integers.
func ParseLoadReport(s string) (LoadReport, error) {
	ds, ns, ok := strings.Cut(s, ",")
	if !ok || !strings.HasPrefix(ds, "d=") || !strings.HasPrefix(ns, "n=") {
		return LoadReport{}, fmt.Errorf("%w: %q", ErrBadLoadReport, s)
	}
	d, err1 := strconv.ParseInt(ds[2:], 10, 64)
	n, err2 := strconv.Atoi(ns[2:])
	if err1 != nil || err2 != nil || d < 0 || n < 0 || d > int64(time.Duration(1<<63-1)/time.Microsecond) {
		return LoadReport{}, fmt.Errorf("%w: %q", ErrBadLoadReport, s)
	}
	return LoadReport{Delay: time.Duration(d) * time.Microsecond, InFlight: n}, nil
}

// LoadMeter measures a full node's load for one request class: POST /tx, the
// requests that light clients route. Counts and times never mix request
// classes, so the report describes exactly the requests being routed.
//
// Model: admission is serialised by the mempool lock, so for this class the
// node behaves approximately as a single FIFO server. A request that arrives
// with a requests already in flight waits for them and then for itself, so
// its sojourn W ≈ (a+1)·s for a per-request service time s. Each finished
// request therefore yields the sample s = W/(a+1); ŝ is their EWMA, and the
// reported delay of a new arrival is D = (n+1)·ŝ for the current in-flight
// count n. n reacts immediately to load from every client; ŝ rises when the
// machine is slower or contended (mining on the same CPU, ledger lock held).
//
// Approximations: Go's mutex is not strictly FIFO, and the parallel part of a
// handler (JSON decoding) slightly inflates ŝ under concurrency.
//
// All operations are O(1) and safe for concurrent use.
type LoadMeter struct {
	now func() time.Time

	mu       sync.Mutex
	inFlight int     // n
	svc      float64 // ŝ in nanoseconds; valid once sampled is true
	sampled  bool
}

// NewLoadMeter returns a meter that reads time from now, or from time.Now if
// now is nil (tests pass a fake clock).
func NewLoadMeter(now func() time.Time) *LoadMeter {
	if now == nil {
		now = time.Now
	}
	return &LoadMeter{now: now}
}

// Start records the arrival of a POST /tx request. Call it at handler entry
// and call the returned function once the response has been written; further
// calls of the returned function do nothing.
func (m *LoadMeter) Start() (done func()) {
	m.mu.Lock()
	ahead := m.inFlight // a: requests in flight at arrival, excluding this one
	m.inFlight++
	start := m.now()
	m.mu.Unlock()

	finished := false
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if finished {
			return
		}
		finished = true
		m.inFlight--
		sojourn := float64(m.now().Sub(start))
		if sojourn < 0 {
			sojourn = 0
		}
		m.observe(sojourn / float64(ahead+1))
	}
}

// observe folds one service-time sample into ŝ; the first sample initialises
// it. Requires m.mu.
func (m *LoadMeter) observe(s float64) {
	if !m.sampled {
		m.svc, m.sampled = s, true
		return
	}
	m.svc += EWMAAlpha * (s - m.svc)
}

// Report returns the current load report D = (n+1)·ŝ, n. It returns false
// until the first POST /tx request has finished, because ŝ is unknown before
// then; a node sends no load header in that case. Reading a report changes
// nothing (GET /tip reads it without being measured).
func (m *LoadMeter) Report() (LoadReport, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.sampled {
		return LoadReport{}, false
	}
	return LoadReport{
		Delay:    time.Duration(float64(m.inFlight+1) * m.svc),
		InFlight: m.inFlight,
	}, true
}
