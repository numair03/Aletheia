package routing

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

// Selector parameters (DR-13). The timeouts are fixed, not adaptive: on a real
// network with variable latency an adaptive timeout produces false failures.
const (
	ProbeInterval  = 1 * time.Second // every node is probed this often
	ProbeTimeout   = 1 * time.Second // limit for one probe
	ConnectTimeout = 2 * time.Second // TCP connect limit, applied by the HTTP client
	RequestTimeout = 5 * time.Second // limit for one request made through Do
	ReportTTL      = 3 * time.Second // an older load report is unknown, never "idle"
	Hysteresis     = 0.2             // δ: the second candidate must be this much better
	FailuresToDown = 2               // consecutive failures that mark a node down
)

// Strategy chooses how a Selector picks a node.
type Strategy int

const (
	// StrategyP2C is the power of two choices over the ring: the ring's
	// candidate or a random second candidate, whichever has the clearly lower
	// estimated response time.
	StrategyP2C Strategy = iota
	// StrategyRing always uses the first healthy ring candidate. It exists so
	// that the two strategies can be compared (--route=ring|p2c).
	StrategyRing
)

// String returns "p2c" or "ring".
func (s Strategy) String() string {
	if s == StrategyRing {
		return "ring"
	}
	return "p2c"
}

// ProbeResult is what one successful probe (GET /tip) of a node returns.
type ProbeResult struct {
	RTT       time.Duration // measured round-trip time
	Report    LoadReport    // the node's load report, if HasReport
	HasReport bool          // false when the node sent no load header yet
}

// ProbeFunc probes one node. ctx carries the probe timeout; an error means
// the node did not answer.
type ProbeFunc func(ctx context.Context, node string) (ProbeResult, error)

// Options configures a Selector. The zero value gives the production
// behaviour with StrategyP2C.
type Options struct {
	Strategy Strategy
	Now      func() time.Time // clock for report ages; default time.Now
	IntN     func(n int) int  // uniform in [0, n); default math/rand/v2.IntN
	TTL      time.Duration    // report lifetime; default ReportTTL
}

// nodeState is what one client knows about one node. Nothing in it is shared
// with other clients.
type nodeState struct {
	failures int  // consecutive failed probes or requests
	down     bool // set after FailuresToDown failures, cleared by a good probe

	rtt    float64 // R: EWMA of probe RTT in nanoseconds; valid if hasRTT
	hasRTT bool

	report    LoadReport // last load report received from the node
	reportAt  time.Time  // when this client received it (client clock)
	hasReport bool
	sentSince int // k: this client's POST /tx sent to the node since report
}

// Selector chooses the full node for a light-client request (DR-13). It
// combines four separate measurements and never conflates them:
//
//   - the node's own POST /tx load, from its load report (LoadMeter);
//   - this client's own POST /tx sends since that report (k), only as a
//     correction to the report, never as a load metric on its own;
//   - the network latency this client observes, as an EWMA R of probe RTTs;
//   - availability, as a health state with failover.
//
// The estimated response time of a node is E = R + (n+k+1)·ŝ, with ŝ = D/(n+1)
// from its report. It is unknown when R is unknown or the report is older
// than the TTL. The ring supplies the default candidate c1 (the first healthy
// node in the key's failover order) and the failover order itself. P2C draws
// a random second healthy candidate c2 per request and uses it only when both
// estimates are known and E(c2) < (1−δ)·E(c1).
//
// Pick is O(log P + N) for P ring points and N nodes; memory is O(N).
// Safe for concurrent use.
type Selector struct {
	ring     *Ring
	strategy Strategy
	now      func() time.Time
	intn     func(int) int
	ttl      time.Duration

	mu    sync.Mutex
	nodes map[string]*nodeState // per-node routing metadata, created on first use
}

// NewSelector returns a selector over ring. Every node starts healthy, with no
// RTT and no report, so until probes arrive it routes like the ring.
func NewSelector(ring *Ring, opt Options) *Selector {
	s := &Selector{
		ring:     ring,
		strategy: opt.Strategy,
		now:      opt.Now,
		intn:     opt.IntN,
		ttl:      opt.TTL,
		nodes:    make(map[string]*nodeState),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.intn == nil {
		s.intn = rand.IntN
	}
	if s.ttl <= 0 {
		s.ttl = ReportTTL
	}
	return s
}

// state returns the state of node, creating it. Requires s.mu.
func (s *Selector) state(node string) *nodeState {
	st := s.nodes[node]
	if st == nil {
		st = &nodeState{}
		s.nodes[node] = st
	}
	return st
}

// Pick returns the node for one request with the given key (e.g. the client
// ID). It returns a healthy node whenever one exists; if every node is down
// it returns the ring owner, as a last resort.
func (s *Selector) Pick(key string) (string, error) {
	cands := s.ring.Candidates(key)
	if len(cands) == 0 {
		return "", ErrEmpty
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pick(cands), nil
}

// pick implements Pick on a candidate snapshot. Requires s.mu.
func (s *Selector) pick(cands []string) string {
	healthy := make([]string, 0, len(cands))
	for _, n := range cands {
		if !s.state(n).down {
			healthy = append(healthy, n)
		}
	}
	if len(healthy) == 0 {
		return cands[0]
	}
	c1 := healthy[0]
	if s.strategy == StrategyRing || len(healthy) < 2 {
		return c1
	}
	c2 := healthy[1+s.intn(len(healthy)-1)] // uniform among the other healthy nodes
	now := s.now()
	e1, ok1 := s.estimate(c1, now)
	e2, ok2 := s.estimate(c2, now)
	if ok1 && ok2 && e2 < (1-Hysteresis)*e1 {
		return c2
	}
	return c1
}

// estimate returns E = R + (n+k+1)·ŝ in nanoseconds, with ŝ = D/(n+1), or
// false if R is unknown or the report is missing or older than the TTL.
// Requires s.mu.
func (s *Selector) estimate(node string, now time.Time) (float64, bool) {
	st := s.state(node)
	if !st.hasRTT || !st.hasReport || now.Sub(st.reportAt) > s.ttl {
		return 0, false
	}
	n := float64(st.report.InFlight)
	svc := float64(st.report.Delay) / (n + 1)
	return st.rtt + (n+float64(st.sentSince)+1)*svc, true
}

// Do sends one request for key: fn is called with the picked node and a
// context limited to RequestTimeout. On failure the failure is recorded and
// the remaining healthy nodes are tried in ring order, then the down ones as
// a last resort. It returns the node that succeeded, or the joined errors.
// If ctx itself ends, Do stops without blaming the node.
//
// fn decides what failure means. A POST /tx callback should call RecordSend
// before sending and RecordReport with the load header of the response.
func (s *Selector) Do(ctx context.Context, key string, fn func(ctx context.Context, node string) error) (string, error) {
	cands := s.ring.Candidates(key)
	if len(cands) == 0 {
		return "", ErrEmpty
	}
	s.mu.Lock()
	first := s.pick(cands)
	order := []string{first}
	for _, wantDown := range []bool{false, true} {
		for _, n := range cands {
			if n != first && s.state(n).down == wantDown {
				order = append(order, n)
			}
		}
	}
	s.mu.Unlock()

	var errs []error
	for _, node := range order {
		rctx, cancel := context.WithTimeout(ctx, RequestTimeout)
		err := fn(rctx, node)
		cancel()
		if err == nil {
			s.recordResult(node, true)
			return node, nil
		}
		if ctx.Err() != nil {
			return "", errors.Join(append(errs, ctx.Err())...)
		}
		s.recordResult(node, false)
		errs = append(errs, errors.New(node+": "+err.Error()))
	}
	return "", errors.Join(errs...)
}

// recordResult updates health after a request. A success resets the failure
// count but does not bring a down node back: only a probe does that.
func (s *Selector) recordResult(node string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordHealth(s.state(node), ok)
}

// recordHealth counts consecutive failures and marks the node down after
// FailuresToDown of them. Requires s.mu.
func (s *Selector) recordHealth(st *nodeState, ok bool) {
	if ok {
		st.failures = 0
		return
	}
	st.failures++
	if st.failures >= FailuresToDown {
		st.down = true
	}
}

// RecordSend notes that this client is sending a POST /tx to node, so the
// node's estimate counts it until the next report arrives (k).
func (s *Selector) RecordSend(node string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state(node).sentSince++
}

// RecordReport stores a load report received from node now (from a POST /tx
// response or a probe). It replaces the previous report and resets k. It does
// not change the node's health.
func (s *Selector) RecordReport(node string, r LoadReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setReport(s.state(node), r)
}

// setReport stores r with the current client time. Requires s.mu.
func (s *Selector) setReport(st *nodeState, r LoadReport) {
	st.report, st.reportAt, st.hasReport, st.sentSince = r, s.now(), true, 0
}

// RecordProbe records the outcome of one probe of node. A successful probe
// marks the node up, adds an RTT sample to R and stores its load report; a
// failed one counts as a failure.
func (s *Selector) RecordProbe(node string, res ProbeResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(node)
	if err != nil {
		s.recordHealth(st, false)
		return
	}
	st.failures, st.down = 0, false
	rtt := float64(res.RTT)
	if !st.hasRTT {
		st.rtt, st.hasRTT = rtt, true
	} else {
		st.rtt += EWMAAlpha * (rtt - st.rtt)
	}
	if res.HasReport {
		s.setReport(st, res.Report)
	}
}

// ProbeOnce probes every node of the ring concurrently, each with a
// ProbeTimeout limit, and records the results. It returns when all probes
// have finished.
func (s *Selector) ProbeOnce(ctx context.Context, probe ProbeFunc) {
	var wg sync.WaitGroup
	for _, node := range s.ring.Nodes() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
			res, err := probe(pctx, node)
			cancel()
			if ctx.Err() != nil {
				return // shutting down: not the node's fault
			}
			s.RecordProbe(node, res, err)
		}()
	}
	wg.Wait()
}

// Run probes every node immediately and then every ProbeInterval until ctx
// ends.
func (s *Selector) Run(ctx context.Context, probe ProbeFunc) {
	t := time.NewTicker(ProbeInterval)
	defer t.Stop()
	for {
		s.ProbeOnce(ctx, probe)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
