package routing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var threeNodes = []string{"node-a", "node-b", "node-c"}

// newTestSelector returns a selector over a 150-vnode ring of nodes with a
// fake clock and the given draw function (nil: always the first other node).
func newTestSelector(strategy Strategy, intn func(int) int, nodes ...string) (*Selector, *fakeClock) {
	if intn == nil {
		intn = func(int) int { return 0 }
	}
	clk := newFakeClock()
	return NewSelector(New(150, nodes...), Options{Strategy: strategy, Now: clk.Now, IntN: intn}), clk
}

// probeOK records a successful probe with the given RTT and report.
func probeOK(s *Selector, node string, rtt, delay time.Duration, n int) {
	s.RecordProbe(node, ProbeResult{RTT: rtt, Report: LoadReport{Delay: delay, InFlight: n}, HasReport: true}, nil)
}

var errDown = errors.New("connection refused")

func markDown(s *Selector, node string) {
	for i := 0; i < FailuresToDown; i++ {
		s.RecordProbe(node, ProbeResult{}, errDown)
	}
}

func isDown(s *Selector, node string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state(node).down
}

func estimateOf(s *Selector, node string) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.estimate(node, s.now())
}

func TestSelectorEmptyRing(t *testing.T) {
	s, _ := newTestSelector(StrategyP2C, nil)
	if _, err := s.Pick("k"); !errors.Is(err, ErrEmpty) {
		t.Fatalf("Pick on empty ring: %v", err)
	}
	if _, err := s.Do(context.Background(), "k", func(context.Context, string) error { return nil }); !errors.Is(err, ErrEmpty) {
		t.Fatalf("Do on empty ring: %v", err)
	}
}

// I2: with no fresh reports (none yet, RTT only, or all expired), Pick is the
// first healthy ring candidate.
func TestSelectorWithoutReportsFollowsRing(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	s, clk := newTestSelector(StrategyP2C, rng.IntN, threeNodes...)
	check := func(phase string) {
		for i := 0; i < 1000; i++ {
			key := fmt.Sprintf("client-%d", i)
			got, err := s.Pick(key)
			if want := s.ring.Candidates(key)[0]; err != nil || got != want {
				t.Fatalf("%s: Pick(%s) = %s %v, want ring owner %s", phase, key, got, err, want)
			}
		}
	}
	check("no state")
	for _, n := range threeNodes {
		s.RecordProbe(n, ProbeResult{RTT: time.Millisecond}, nil) // RTT, no report
	}
	check("RTT only")
	probeOK(s, "node-a", time.Millisecond, 100*time.Millisecond, 50)
	probeOK(s, "node-b", time.Millisecond, time.Microsecond, 0)
	probeOK(s, "node-c", time.Millisecond, time.Microsecond, 0)
	clk.Advance(ReportTTL + time.Nanosecond)
	check("expired reports")
}

// I1: a healthy node is returned whenever one exists: the first healthy
// candidate when there are no reports; the ring owner if all are down.
func TestSelectorReturnsHealthyNode(t *testing.T) {
	nodes := []string{"n1", "n2", "n3", "n4"}
	for mask := 0; mask < 1<<len(nodes); mask++ { // bit set = node down
		rng := rand.New(rand.NewPCG(uint64(mask), 9))
		s, _ := newTestSelector(StrategyP2C, rng.IntN, nodes...)
		for i, n := range nodes {
			probeOK(s, n, time.Duration(1+rng.IntN(5))*time.Millisecond, time.Duration(rng.IntN(50))*time.Millisecond, rng.IntN(5))
			if mask&(1<<i) != 0 {
				markDown(s, n)
			}
		}
		for k := 0; k < 200; k++ {
			key := fmt.Sprintf("key-%d", k)
			got, _ := s.Pick(key)
			cands := s.ring.Candidates(key)
			if mask == 1<<len(nodes)-1 {
				if got != cands[0] {
					t.Fatalf("all down: Pick = %s, want ring owner %s", got, cands[0])
				}
			} else if isDown(s, got) {
				t.Fatalf("mask %04b: Pick(%s) = down node %s", mask, key, got)
			}
		}
	}
}

// pickSetup gives c1 an estimate of 11ms (RTT 1ms + D 10ms, n=0) and c2 the
// estimate e2, and returns the two candidates.
func pickSetup(t *testing.T, s *Selector, key string, e2 time.Duration) (c1, c2 string) {
	t.Helper()
	cands := s.ring.Candidates(key)
	c1, c2 = cands[0], cands[1]
	for _, n := range cands[2:] {
		markDown(s, n) // only c1 and c2 are healthy, so c2 is the only draw
	}
	probeOK(s, c1, time.Millisecond, 10*time.Millisecond, 0)
	probeOK(s, c2, time.Millisecond, e2-time.Millisecond, 0)
	return c1, c2
}

// I3: the result is c1 or c2, and c2 only if E(c2) < (1−δ)·E(c1).
func TestSelectorHysteresis(t *testing.T) {
	margin := time.Duration((1 - Hysteresis) * float64(11*time.Millisecond)) // 8.8ms
	for _, tc := range []struct {
		e2      time.Duration
		wantC2  bool
		comment string
	}{
		{margin + time.Microsecond, false, "just inside the margin"},
		{margin - time.Microsecond, true, "just better than the margin"},
		{10 * time.Millisecond, false, "better, but by less than δ"},
		{2 * time.Millisecond, true, "much better"},
		{50 * time.Millisecond, false, "worse"},
	} {
		s, _ := newTestSelector(StrategyP2C, nil, threeNodes...)
		c1, c2 := pickSetup(t, s, "client-1", tc.e2)
		want := c1
		if tc.wantC2 {
			want = c2
		}
		if got, _ := s.Pick("client-1"); got != want {
			t.Errorf("%s (E2=%v): Pick = %s, want %s", tc.comment, tc.e2, got, want)
		}
	}
}

// I3 over random states: every pick is c1 or the drawn c2, and c2 only when
// its estimate is better by the margin.
func TestSelectorPickIsC1OrBetterC2(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	nodes := []string{"n1", "n2", "n3", "n4", "n5"}
	var drawn int
	s, _ := newTestSelector(StrategyP2C, func(n int) int { drawn = rng.IntN(n); return drawn }, nodes...)
	for i := 0; i < 5000; i++ {
		n := nodes[rng.IntN(len(nodes))]
		switch rng.IntN(4) {
		case 0:
			s.RecordProbe(n, ProbeResult{}, errDown)
		default:
			probeOK(s, n, time.Duration(1+rng.IntN(20))*time.Millisecond, time.Duration(rng.IntN(100))*time.Millisecond, rng.IntN(10))
		}
		key := fmt.Sprintf("client-%d", rng.IntN(50))
		var healthy []string
		for _, c := range s.ring.Candidates(key) {
			if !isDown(s, c) {
				healthy = append(healthy, c)
			}
		}
		drawn = -1
		got, _ := s.Pick(key)
		if len(healthy) < 2 {
			continue
		}
		c1 := healthy[0]
		if got == c1 {
			continue
		}
		if drawn < 0 || got != healthy[1+drawn] {
			t.Fatalf("step %d: Pick = %s, neither c1 %s nor the drawn c2", i, got, c1)
		}
		e1, ok1 := estimateOf(s, c1)
		e2, ok2 := estimateOf(s, got)
		if !ok1 || !ok2 || !(e2 < (1-Hysteresis)*e1) {
			t.Fatalf("step %d: chose c2 %s with E1=%v(%v) E2=%v(%v)", i, got, e1, ok1, e2, ok2)
		}
	}
}

func TestSelectorRingStrategyIgnoresLoad(t *testing.T) {
	s, _ := newTestSelector(StrategyRing, nil, threeNodes...)
	c1, _ := pickSetup(t, s, "client-1", 2*time.Millisecond)
	if got, _ := s.Pick("client-1"); got != c1 {
		t.Fatalf("ring strategy picked %s, want c1 %s", got, c1)
	}
	if StrategyRing.String() != "ring" || StrategyP2C.String() != "p2c" {
		t.Fatal("strategy names")
	}
}

// I6: a report is used up to the TTL and never after it.
func TestSelectorReportTTL(t *testing.T) {
	s, clk := newTestSelector(StrategyP2C, nil, threeNodes...)
	c1, c2 := pickSetup(t, s, "client-1", 2*time.Millisecond)
	clk.Advance(ReportTTL)
	if got, _ := s.Pick("client-1"); got != c2 {
		t.Fatalf("report aged exactly TTL not used: Pick = %s, want c2 %s", got, c2)
	}
	clk.Advance(time.Nanosecond)
	if got, _ := s.Pick("client-1"); got != c1 {
		t.Fatalf("expired report used: Pick = %s, want c1 %s", got, c1)
	}
	if _, ok := estimateOf(s, c2); ok {
		t.Fatal("estimate of an expired report is known")
	}
	// Fresh again, from POST /tx responses. Both reports must be fresh: with
	// c1's estimate unknown the selector keeps c1.
	s.RecordReport(c2, LoadReport{Delay: time.Millisecond})
	if got, _ := s.Pick("client-1"); got != c1 {
		t.Fatalf("c1 report expired, yet Pick = %s, want c1 %s", got, c1)
	}
	s.RecordReport(c1, LoadReport{Delay: 10 * time.Millisecond})
	if got, _ := s.Pick("client-1"); got != c2 {
		t.Fatalf("refreshed report not used: Pick = %s", got)
	}
}

// E = R + (n+k+1)·ŝ with ŝ = D/(n+1); k counts this client's sends since the
// report and is reset by the next report. R is an EWMA of probe RTTs.
func TestSelectorEstimate(t *testing.T) {
	s, _ := newTestSelector(StrategyP2C, nil, "x")
	probeOK(s, "x", 10*time.Millisecond, 30*time.Millisecond, 2) // ŝ = 10ms
	want := func(d time.Duration) {
		t.Helper()
		if e, ok := estimateOf(s, "x"); !ok || time.Duration(e) != d {
			t.Fatalf("E = %v (%v), want %v", time.Duration(e), ok, d)
		}
	}
	want(40 * time.Millisecond)
	s.RecordSend("x")
	s.RecordSend("x")
	want(60 * time.Millisecond)
	s.RecordReport("x", LoadReport{Delay: 30 * time.Millisecond, InFlight: 2})
	want(40 * time.Millisecond)
	s.RecordProbe("x", ProbeResult{RTT: 20 * time.Millisecond}, nil) // R = 10 + 0.2·10 = 12ms; report kept
	want(42 * time.Millisecond)
}

// The k correction: a client that keeps sending to the better node raises its
// own estimate of it, and returns to c1 before the next report arrives.
func TestSelectorOwnSendsCorrectStaleReport(t *testing.T) {
	s, _ := newTestSelector(StrategyP2C, nil, threeNodes...)
	_, c2 := pickSetup(t, s, "client-1", 2*time.Millisecond) // c2: R 1ms, ŝ 1ms
	picks := 0
	for i := 0; i < 20; i++ {
		got, _ := s.Pick("client-1")
		if got != c2 {
			break
		}
		picks++
		s.RecordSend(c2)
	}
	// c2 stays chosen while 1ms + (k+1)·1ms < 8.8ms, i.e. for k = 0..6.
	if picks != 7 {
		t.Fatalf("c2 chosen %d times before switching back, want 7", picks)
	}
}

// I7: down after FailuresToDown consecutive failures (probe or request), up
// after one good probe; a successful request alone does not bring it back.
func TestSelectorHealth(t *testing.T) {
	s, _ := newTestSelector(StrategyP2C, nil, threeNodes...)
	n := "node-b"
	s.RecordProbe(n, ProbeResult{}, errDown)
	if isDown(s, n) {
		t.Fatal("down after one failure")
	}
	s.recordResult(n, true) // a success resets the count
	s.RecordProbe(n, ProbeResult{}, errDown)
	if isDown(s, n) {
		t.Fatal("failures not consecutive, but node is down")
	}
	s.recordResult(n, false) // request failure is the second consecutive one
	if !isDown(s, n) {
		t.Fatal("not down after two consecutive failures")
	}
	s.recordResult(n, true)
	if !isDown(s, n) {
		t.Fatal("a successful request brought a down node back")
	}
	s.RecordProbe(n, ProbeResult{RTT: time.Millisecond}, nil)
	if isDown(s, n) {
		t.Fatal("still down after a good probe")
	}
	s.RecordProbe(n, ProbeResult{}, errDown)
	if isDown(s, n) {
		t.Fatal("good probe did not reset the failure count")
	}
}

func TestSelectorDoFailover(t *testing.T) {
	key := "client-7"
	run := func(s *Selector, failing ...string) (string, []string, error) {
		var calls []string
		node, err := s.Do(context.Background(), key, func(ctx context.Context, node string) error {
			calls = append(calls, node)
			if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > RequestTimeout {
				t.Fatalf("request context deadline %v %v", dl, ok)
			}
			if slices.Contains(failing, node) {
				return errDown
			}
			return nil
		})
		return node, calls, err
	}

	s, _ := newTestSelector(StrategyP2C, nil, threeNodes...)
	c := s.ring.Candidates(key)
	if node, calls, err := run(s); err != nil || node != c[0] || len(calls) != 1 {
		t.Fatalf("healthy: %s %v %v", node, calls, err)
	}
	if node, calls, err := run(s, c[0]); err != nil || node != c[1] || !slices.Equal(calls, c[:2]) {
		t.Fatalf("owner failing: %s %v %v", node, calls, err)
	}
	// The second failure marks c[0] down; c[1] is then tried first, and the
	// down node only as a last resort, after the healthy ones.
	run(s, c[0])
	if !isDown(s, c[0]) {
		t.Fatal("two failed requests did not mark the node down")
	}
	if node, calls, err := run(s, c[1], c[2]); err != nil || node != c[0] || !slices.Equal(calls, []string{c[1], c[2], c[0]}) {
		t.Fatalf("down node as last resort: %s %v %v", node, calls, err)
	}

	_, calls, err := run(s, threeNodes...)
	if err == nil || len(calls) != 3 {
		t.Fatalf("all failing: %v %v", calls, err)
	}
	for _, n := range threeNodes {
		if !strings.Contains(err.Error(), n+": ") {
			t.Fatalf("error does not name %s: %v", n, err)
		}
	}
}

// When the caller's own context ends, Do stops and does not blame the node.
func TestSelectorDoCallerCancel(t *testing.T) {
	s, _ := newTestSelector(StrategyP2C, nil, threeNodes...)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := s.Do(ctx, "k", func(ctx context.Context, node string) error {
		calls++
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("Do after cancel: calls %d, err %v", calls, err)
	}
	for _, n := range threeNodes {
		s.mu.Lock()
		f := s.state(n).failures
		s.mu.Unlock()
		if f != 0 {
			t.Fatalf("%s blamed for the caller's cancel", n)
		}
	}
}

func TestSelectorProbeOnceAndRun(t *testing.T) {
	s, _ := newTestSelector(StrategyP2C, nil, threeNodes...)
	var mu sync.Mutex
	probed := map[string]int{}
	probe := func(ctx context.Context, node string) (ProbeResult, error) {
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > ProbeTimeout {
			return ProbeResult{}, errors.New("probe without the probe timeout")
		}
		mu.Lock()
		probed[node]++
		mu.Unlock()
		if node == "node-c" {
			return ProbeResult{}, errDown
		}
		return ProbeResult{RTT: 2 * time.Millisecond, Report: LoadReport{Delay: 4 * time.Millisecond}, HasReport: true}, nil
	}
	s.ProbeOnce(context.Background(), probe)
	if _, ok := estimateOf(s, "node-a"); !ok || isDown(s, "node-c") {
		t.Fatal("after one probe round: node-a estimate unknown or node-c already down")
	}
	s.ProbeOnce(context.Background(), probe)
	if !isDown(s, "node-c") || isDown(s, "node-a") {
		t.Fatal("after two probe rounds: node-c should be down, node-a up")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.ProbeOnce(ctx, func(context.Context, string) (ProbeResult, error) { return ProbeResult{}, errDown })
	if isDown(s, "node-a") {
		t.Fatal("probes cancelled by shutdown counted as failures")
	}

	// Run probes immediately, then stops when its context ends.
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, probe); close(done) }()
	for {
		mu.Lock()
		n := probed["node-a"]
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

// Every operation used at once, with membership changes, under -race.
func TestSelectorConcurrent(t *testing.T) {
	ring := New(50, threeNodes...)
	s := NewSelector(ring, Options{})
	probe := func(ctx context.Context, node string) (ProbeResult, error) {
		if node == "node-b" {
			return ProbeResult{}, errDown
		}
		return ProbeResult{RTT: time.Millisecond, Report: LoadReport{Delay: time.Millisecond, InFlight: 1}, HasReport: true}, nil
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("c%d-%d", g, i)
				s.Pick(key)
				s.Do(context.Background(), key, func(ctx context.Context, node string) error {
					s.RecordSend(node)
					s.RecordReport(node, LoadReport{Delay: time.Millisecond, InFlight: i % 4})
					return nil
				})
				if i%50 == 0 {
					s.ProbeOnce(context.Background(), probe)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			ring.Add("node-d")
			ring.Remove("node-d")
		}
	}()
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Deterministic multi-client simulation (DR-13 evaluation).
//
// N = 3 FIFO single servers with service times 1:1:4 ms, each measured by a
// real LoadMeter on simulated time; K = 20 independent clients, each with its
// own Selector, Poisson arrivals at 1.6 requests/ms in total (71% of the
// 2.25 requests/ms capacity), 0.2 ms RTT to every server. Every T all
// clients probe all servers at the same instant, so they all read the same,
// increasingly stale reports; no other reports are delivered.
//
// Policies:
//   - simRing: StrategyRing;
//   - simLeastBoard: global least-loaded on the reports alone (the classic
//     stale-information model: no client counts its own sends);
//   - simLeast: global least-loaded on the selector's estimate E, including
//     each client's own sends since the report (k);
//   - simP2C: StrategyP2C.
//
// The least-loaded policies are not part of the product (DR-13 rejects them);
// they exist only here, for comparison.
//
// Measured: p99 response time; the max/mean ratio of the servers' average
// in-flight counts (sampled at Poisson arrivals, so time averages); and
// herding: for each 10 ms window, the largest share of that window's
// arrivals that went to a single server, averaged over windows (about 0.45
// for a capacity-proportional split, 1 when everyone picks the same server).

const (
	simRing       = "ring"
	simLeastBoard = "least-board"
	simLeast      = "least-k"
	simP2C        = "p2c"
	simRTT        = 200 * time.Microsecond
	simWindow     = 10 * time.Millisecond
)

type simJob struct {
	finish time.Time
	done   func()
}

type simServer struct {
	name      string
	svc       time.Duration
	clock     time.Time // the meter's clock; never moves backwards
	meter     *LoadMeter
	busyUntil time.Time
	queue     []simJob // in flight, FIFO; finish times increase
	sumN      float64  // sum of in-flight counts seen by arrivals
	served    int
}

// advance completes every job that finishes by t, then moves the clock to t.
func (sv *simServer) advance(t time.Time) {
	for len(sv.queue) > 0 && !sv.queue[0].finish.After(t) {
		sv.clock = sv.queue[0].finish
		sv.queue[0].done()
		sv.queue = sv.queue[1:]
	}
	sv.clock = t
}

// arrive admits a job at t (after advance(t)) and returns its finish time.
func (sv *simServer) arrive(t time.Time) time.Time {
	done := sv.meter.Start()
	start := t
	if sv.busyUntil.After(t) {
		start = sv.busyUntil
	}
	sv.busyUntil = start.Add(sv.svc)
	sv.queue = append(sv.queue, simJob{finish: sv.busyUntil, done: done})
	sv.served++
	return sv.busyUntil
}

type simResult struct {
	p99     time.Duration
	maxMean float64
	herd    float64
	served  map[string]int
}

func runSim(policy string, refresh time.Duration, seed uint64) simResult {
	const (
		clients  = 20
		duration = 20 * time.Second
		rate     = 1.6 // requests per ms, all clients together
	)
	t0 := time.Unix(0, 0)
	now := t0
	clock := func() time.Time { return now }

	servers := []*simServer{
		{name: "node-a", svc: time.Millisecond},
		{name: "node-b", svc: time.Millisecond},
		{name: "node-slow", svc: 4 * time.Millisecond},
	}
	index := map[string]int{}
	var names []string
	for i, sv := range servers {
		sv.clock = t0
		sv.meter = NewLoadMeter(func() time.Time { return sv.clock })
		index[sv.name] = i
		names = append(names, sv.name)
	}

	strategy := StrategyP2C
	if policy == simRing {
		strategy = StrategyRing
	}
	type client struct {
		key  string
		sel  *Selector
		rng  *rand.Rand
		next time.Time
	}
	expo := func(rng *rand.Rand) time.Duration {
		return time.Duration(rng.ExpFloat64() / (rate / clients) * float64(time.Millisecond))
	}
	cs := make([]*client, clients)
	for i := range cs {
		rng := rand.New(rand.NewPCG(seed, uint64(i)))
		cs[i] = &client{
			key: fmt.Sprintf("client-%d", i),
			sel: NewSelector(New(150, names...), Options{Strategy: strategy, Now: clock, IntN: rng.IntN}),
			rng: rng,
		}
		cs[i].next = t0.Add(expo(rng))
	}

	// leastLoaded returns the healthy node with the smallest estimate, or the
	// ring default while any estimate is unknown.
	leastLoaded := func(s *Selector, key string) string {
		cands := s.ring.Candidates(key)
		s.mu.Lock()
		defer s.mu.Unlock()
		best, bestE := "", math.Inf(1)
		for _, n := range cands {
			if s.state(n).down {
				continue
			}
			e, ok := s.estimate(n, now)
			if !ok {
				return s.pick(cands)
			}
			if e < bestE {
				best, bestE = n, e
			}
		}
		return best
	}

	var resp []time.Duration
	var windows [][3]int // arrivals per server in each simWindow
	// The first probe round comes after a short warm-up, so that the meters
	// have samples: real nodes serve continuously, and a cold start would
	// route ring-only for a whole period and overload the slow server.
	nextProbe := t0.Add(10 * time.Millisecond)
	end := t0.Add(duration)
	for {
		c := cs[0]
		for _, x := range cs[1:] {
			if x.next.Before(c.next) {
				c = x
			}
		}
		if !nextProbe.After(c.next) { // probe round first: every client reads the same reports
			now = nextProbe
			for _, sv := range servers {
				sv.advance(now)
			}
			for _, x := range cs {
				for _, sv := range servers {
					r, ok := sv.meter.Report()
					x.sel.RecordProbe(sv.name, ProbeResult{RTT: simRTT, Report: r, HasReport: ok}, nil)
				}
			}
			nextProbe = nextProbe.Add(refresh)
			continue
		}
		now = c.next
		if !now.Before(end) {
			break
		}
		for _, sv := range servers {
			sv.advance(now)
			sv.sumN += float64(len(sv.queue))
		}
		var node string
		switch policy {
		case simLeastBoard, simLeast:
			node = leastLoaded(c.sel, c.key)
		default:
			node, _ = c.sel.Pick(c.key)
		}
		if policy != simLeastBoard { // board-only: k stays 0, so E = R + D
			c.sel.RecordSend(node)
		}
		sv := servers[index[node]]
		resp = append(resp, sv.arrive(now).Sub(now)+simRTT)
		w := int(now.Sub(t0) / simWindow)
		for len(windows) <= w {
			windows = append(windows, [3]int{})
		}
		windows[w][index[node]]++
		c.next = now.Add(expo(c.rng))
	}

	slices.Sort(resp)
	res := simResult{p99: resp[len(resp)*99/100], served: map[string]int{}}
	var maxN, sumN float64
	for _, sv := range servers {
		avg := sv.sumN / float64(len(resp))
		maxN = max(maxN, avg)
		sumN += avg
		res.served[sv.name] = sv.served
	}
	res.maxMean = maxN / (sumN / float64(len(servers)))
	var shares float64
	var counted int
	for _, w := range windows {
		if total := w[0] + w[1] + w[2]; total > 0 {
			shares += float64(max(w[0], w[1], w[2])) / float64(total)
			counted++
		}
	}
	res.herd = shares / float64(counted)
	return res
}

func TestRoutingSimulation(t *testing.T) {
	refreshes := []time.Duration{10 * time.Millisecond, 100 * time.Millisecond, time.Second}
	res := map[string]map[time.Duration]simResult{}
	for _, policy := range []string{simRing, simLeastBoard, simLeast, simP2C} {
		res[policy] = map[time.Duration]simResult{}
		for _, T := range refreshes {
			r := runSim(policy, T, 42)
			res[policy][T] = r
			t.Logf("%-11s T=%-5v p99=%-14v max/mean in-flight=%4.2f herding=%4.2f served a/b/slow=%d/%d/%d",
				policy, T, r.p99, r.maxMean, r.herd, r.served["node-a"], r.served["node-b"], r.served["node-slow"])
		}
	}
	for i, T := range refreshes {
		ring, board, least, p2c := res[simRing][T], res[simLeastBoard][T], res[simLeast][T], res[simP2C][T]
		// P2C beats ring-only with heterogeneous servers.
		if !(p2c.p99 < ring.p99) || !(p2c.maxMean < ring.maxMean) {
			t.Errorf("T=%v: P2C (p99 %v, max/mean %.2f) does not beat ring-only (p99 %v, max/mean %.2f)",
				T, p2c.p99, p2c.maxMean, ring.p99, ring.maxMean)
		}
		if p2c.served["node-slow"] >= ring.served["node-slow"] {
			t.Errorf("T=%v: P2C sends no less to the slow node (%d) than ring-only (%d)",
				T, p2c.served["node-slow"], ring.served["node-slow"])
		}
		// Board-only least-loaded herds: in almost every window all clients
		// pick the same server.
		if board.herd < 0.9 {
			t.Errorf("T=%v: board-only least-loaded herding %.2f, want ≥ 0.9", T, board.herd)
		}
		// P2C herds less than board-only least-loaded, and less than the
		// k-corrected one wherever that one herds. With long periods a client
		// sends many requests per report, and k alone spreads its load, so
		// k-corrected least-loaded stops herding; that case is only logged.
		if !(p2c.herd < board.herd) {
			t.Errorf("T=%v: P2C herding %.2f not below board-only least-loaded %.2f", T, p2c.herd, board.herd)
		}
		if least.herd >= 0.9 && !(p2c.herd < least.herd) {
			t.Errorf("T=%v: P2C herding %.2f not below k-corrected least-loaded %.2f", T, p2c.herd, least.herd)
		}
		// Board-only least-loaded gets worse as the reports get older.
		if i > 0 && !(board.p99 > res[simLeastBoard][refreshes[i-1]].p99) {
			t.Errorf("board-only least-loaded p99 does not grow from T=%v (%v) to T=%v (%v)",
				refreshes[i-1], res[simLeastBoard][refreshes[i-1]].p99, T, board.p99)
		}
	}
}
