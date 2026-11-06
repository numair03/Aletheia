package routing

import (
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for deterministic tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestLoadMeterNoReportBeforeFirstSample(t *testing.T) {
	clk := newFakeClock()
	m := NewLoadMeter(clk.Now)
	if _, ok := m.Report(); ok {
		t.Fatal("report before any request")
	}
	done := m.Start()
	if _, ok := m.Report(); ok {
		t.Fatal("report while the first request is still in flight")
	}
	clk.Advance(3 * time.Millisecond)
	done()
	r, ok := m.Report()
	if !ok || r.InFlight != 0 || r.Delay != 3*time.Millisecond {
		t.Fatalf("after one 3ms request: %+v %v, want d=3ms n=0", r, ok)
	}
}

// A FIFO single server with service time s, fed a scripted burst: the
// request that finds a requests ahead has sojourn (a+1)·s, so every sample
// W/(a+1) equals s exactly and the meter reports D = (n+1)·s.
func TestLoadMeterScriptedFIFOQueue(t *testing.T) {
	const s = 4 * time.Millisecond
	clk := newFakeClock()
	m := NewLoadMeter(clk.Now)

	var dones []func()
	for i := 0; i < 5; i++ { // five arrivals at t=0, ahead = 0..4
		dones = append(dones, m.Start())
	}
	for i, done := range dones { // FIFO completions at s, 2s, ..., 5s
		clk.Advance(s)
		done()
		r, ok := m.Report()
		wantN := len(dones) - i - 1
		if !ok || r.InFlight != wantN || r.Delay != time.Duration(wantN+1)*s {
			t.Fatalf("after completion %d: %+v, want n=%d d=%v", i, r, wantN, time.Duration(wantN+1)*s)
		}
	}

	// n reacts at once to new arrivals; ŝ does not change until they finish.
	d1, d2 := m.Start(), m.Start()
	if r, _ := m.Report(); r.InFlight != 2 || r.Delay != 3*s {
		t.Fatalf("two in flight: %+v, want n=2 d=%v", r, 3*s)
	}
	clk.Advance(s)
	d1()
	clk.Advance(s)
	d2()
	if r, _ := m.Report(); r.InFlight != 0 || r.Delay != s {
		t.Fatalf("drained: %+v, want n=0 d=%v", r, s)
	}
}

// A slower server (contention, weaker machine) shows up in ŝ, which moves
// towards the new service time by α per sample.
func TestLoadMeterEWMATracksServiceTime(t *testing.T) {
	clk := newFakeClock()
	m := NewLoadMeter(clk.Now)
	serve := func(d time.Duration) {
		done := m.Start()
		clk.Advance(d)
		done()
	}
	serve(10 * time.Millisecond)
	serve(20 * time.Millisecond)
	want := 10*time.Millisecond + time.Duration(EWMAAlpha*float64(10*time.Millisecond))
	if r, _ := m.Report(); r.Delay != want {
		t.Fatalf("EWMA after 10ms, 20ms = %v, want %v", r.Delay, want)
	}
	for i := 0; i < 100; i++ {
		serve(20 * time.Millisecond)
	}
	if r, _ := m.Report(); r.Delay < 19*time.Millisecond || r.Delay > 20*time.Millisecond {
		t.Fatalf("EWMA did not converge to 20ms: %v", r.Delay)
	}
}

// I5: the service estimate always lies between the smallest and the largest
// sample seen.
func TestLoadMeterEWMABounds(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	clk := newFakeClock()
	m := NewLoadMeter(clk.Now)
	lo, hi := time.Duration(1<<62), time.Duration(0)
	for i := 0; i < 2000; i++ {
		d := time.Duration(1+rng.IntN(50_000)) * time.Microsecond
		done := m.Start()
		clk.Advance(d)
		done()
		lo, hi = min(lo, d), max(hi, d)
		r, _ := m.Report() // n = 0, so D = ŝ
		if r.Delay < lo || r.Delay > hi {
			t.Fatalf("step %d: ŝ = %v outside [%v, %v]", i, r.Delay, lo, hi)
		}
	}
}

// I4 at the meter: only Start/done (the POST /tx middleware) change the
// estimate; reading reports, as GET /tip does, never does.
func TestLoadMeterReadingDoesNotMeasure(t *testing.T) {
	clk := newFakeClock()
	m := NewLoadMeter(clk.Now)
	done := m.Start()
	clk.Advance(2 * time.Millisecond)
	done()
	before, _ := m.Report()
	for i := 0; i < 50; i++ {
		clk.Advance(time.Second)
		m.Report()
	}
	if after, _ := m.Report(); after != before {
		t.Fatalf("reads changed the report: %+v → %+v", before, after)
	}
}

func TestLoadMeterDoneIsIdempotent(t *testing.T) {
	clk := newFakeClock()
	m := NewLoadMeter(clk.Now)
	keep := m.Start()
	done := m.Start()
	clk.Advance(time.Millisecond)
	done()
	done()
	if r, _ := m.Report(); r.InFlight != 1 {
		t.Fatalf("second done changed n: %+v", r)
	}
	keep()
}

func TestLoadMeterConcurrent(t *testing.T) {
	m := NewLoadMeter(nil)
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				done := m.Start()
				m.Report()
				done()
			}
		}()
	}
	wg.Wait()
	if r, ok := m.Report(); !ok || r.InFlight != 0 {
		t.Fatalf("after all requests: %+v %v, want n=0", r, ok)
	}
}

func TestLoadReportHeaderFormat(t *testing.T) {
	r := LoadReport{Delay: 1234567 * time.Nanosecond, InFlight: 7}
	if got := r.String(); got != "d=1234,n=7" {
		t.Fatalf("String = %q", got)
	}
	for _, want := range []LoadReport{{0, 0}, {time.Microsecond, 1}, {90 * time.Second, 12345}} {
		got, err := ParseLoadReport(want.String())
		if err != nil || got != want {
			t.Fatalf("round trip %+v: %+v %v", want, got, err)
		}
	}
	for _, bad := range []string{"", "d=1", "n=1,d=1", "d=,n=1", "d=1,n=", "d=-1,n=0", "d=1,n=-2",
		"d=1,n=2,x=3", "d=1.5,n=2", "D=1,n=2", " d=1,n=2", "d=99999999999999999999,n=0", "d=9223372036854776,n=0"} {
		if _, err := ParseLoadReport(bad); !errors.Is(err, ErrBadLoadReport) {
			t.Errorf("ParseLoadReport(%q) error = %v, want ErrBadLoadReport", bad, err)
		}
	}
}
