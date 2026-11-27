package route

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fabiolb/fabio/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// mustLookup returns the target for the given host and path and fails the
// test on a panic in the picker.
func mustLookup(t *testing.T, tbl Table, host, path string, pick picker) *Target {
	t.Helper()
	var target *Target
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Lookup(%q, %q) panicked: %v", host, path, r)
			}
		}()
		req := &http.Request{Host: host, URL: mustParse(path)}
		target = tbl.Lookup(req, pick, prefixMatcher, globCache, globDisabled)
	}()
	return target
}

func findTarget(r *Route, url string) *Target {
	for _, t := range r.Targets {
		if t.URL.String() == url {
			return t
		}
	}
	return nil
}

// TestZeroWeightNoTraffic verifies that a target with weight 0 receives no
// traffic while a dynamic target on the same route receives all of it.
func TestZeroWeightNoTraffic(t *testing.T) {
	tbl, err := NewTable(bytes.NewBufferString(`
route add a foo.com/ http://a:80/ weight 0
route add b foo.com/ http://b:80/
`))
	if err != nil {
		t.Fatal(err)
	}

	r := tbl["foo.com"][0]
	if got, want := len(r.wTargets), int(maxSlots); got != want {
		t.Fatalf("wTargets: got %d want %d", got, want)
	}
	b := findTarget(r, "http://b:80/")
	if b == nil {
		t.Fatal("target b missing")
	}
	if got, want := b.Weight, 1.0; got != want {
		t.Errorf("b.Weight: got %v want %v", got, want)
	}
	a := findTarget(r, "http://a:80/")
	if a == nil {
		t.Fatal("target a missing")
	}
	if got, want := a.Weight, 0.0; got != want {
		t.Errorf("a.Weight: got %v want %v", got, want)
	}

	for i := 0; i < maxSlots; i++ {
		tg := mustLookup(t, tbl, "foo.com", "/x", rrPicker)
		if tg == nil || tg.URL.String() != "http://b:80/" {
			t.Fatalf("lookup %d: got %v want http://b:80/", i, tg)
		}
	}
}

// TestZeroWeightPartialFraction verifies the exact slot distribution for
// 0.0001/0.9999.
func TestZeroWeightPartialFraction(t *testing.T) {
	tbl, err := NewTable(bytes.NewBufferString(`
route add a foo.com/ http://a:80/ weight 0.0001
route add b foo.com/ http://b:80/ weight 0.9999
`))
	if err != nil {
		t.Fatal(err)
	}

	var aCount, bCount int
	for i := 0; i < maxSlots; i++ {
		switch tg := mustLookup(t, tbl, "foo.com", "/x", rrPicker); {
		case tg == nil:
			t.Fatalf("lookup %d: got nil", i)
		case tg.URL.String() == "http://a:80/":
			aCount++
		case tg.URL.String() == "http://b:80/":
			bCount++
		default:
			t.Fatalf("lookup %d: unexpected target %v", i, tg)
		}
	}
	if aCount != 1 || bCount != 9999 {
		t.Errorf("got a=%d b=%d want a=1 b=9999", aCount, bCount)
	}
}

// TestZeroWeightAllDrainedNoFallback verifies that a route with all targets
// drained does not fall through to less specific paths, other hosts or the
// hostless catch-all route.
func TestZeroWeightAllDrainedNoFallback(t *testing.T) {
	tbl, err := NewTable(bytes.NewBufferString(`
route add a foo.com/ http://a:80/ weight 0
route add b foo.com/ http://b:80/ weight 0
route add other / http://10.9.0.1:80/
`))
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range []picker{rrPicker, rndPicker} {
		if tg := mustLookup(t, tbl, "foo.com", "/x", p); tg != nil {
			t.Errorf("%T: got %v want nil", p, tg)
		}
	}

	// a different host still matches the catch-all route
	if tg := mustLookup(t, tbl, "bar.com", "/x", rrPicker); tg == nil || tg.URL.String() != "http://10.9.0.1:80/" {
		t.Errorf("catch-all: got %v want http://10.9.0.1:80/", tg)
	}
	if tg := mustLookup(t, tbl, "", "/x", rrPicker); tg == nil || tg.URL.String() != "http://10.9.0.1:80/" {
		t.Errorf("hostless: got %v want http://10.9.0.1:80/", tg)
	}
}

// TestZeroWeightMoreSpecificDrained verifies that a drained more specific
// path does not fall through to a normal less specific path of the same host.
func TestZeroWeightMoreSpecificDrained(t *testing.T) {
	tbl, err := NewTable(bytes.NewBufferString(`
route add a foo.com/api http://a:80/ weight 0
route add b foo.com/ http://b:80/
`))
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range []picker{rrPicker, rndPicker} {
		if tg := mustLookup(t, tbl, "foo.com", "/api/x", p); tg != nil {
			t.Errorf("%T: got %v want nil", p, tg)
		}
	}
	// other paths on the same host still use the less specific route
	if tg := mustLookup(t, tbl, "foo.com", "/other", rrPicker); tg == nil || tg.URL.String() != "http://b:80/" {
		t.Errorf("got %v want http://b:80/", tg)
	}
}

// TestZeroWeightSingleTarget verifies that a single drained target receives
// no traffic as well. This changes the previous behavior where the single
// target shortcut returned the target regardless of its weight.
func TestZeroWeightSingleTarget(t *testing.T) {
	tbl, err := NewTable(bytes.NewBufferString(`
route add a foo.com/ http://a:80/ weight 0
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []picker{rrPicker, rndPicker} {
		if tg := mustLookup(t, tbl, "foo.com", "/x", p); tg != nil {
			t.Errorf("%T: got %v want nil", p, tg)
		}
	}
	if tg := tbl.LookupHost("foo.com", rrPicker); tg != nil {
		t.Errorf("LookupHost: got %v want nil", tg)
	}
}

// TestNegativeZeroWeightIsZero verifies that weight -0 behaves like weight 0
// and that the generated config does not contain a negative sign.
func TestNegativeZeroWeightIsZero(t *testing.T) {
	negZero := -0.0

	tbl, err := NewTable(bytes.NewBufferString("route add svc / http://10.0.0.1:80/ weight -0"))
	if err != nil {
		t.Fatal(err)
	}
	r := tbl[""][0]
	if got := r.Targets[0].FixedWeight; got != 0 || negZero != 0 {
		t.Errorf("FixedWeight: got %v want 0", got)
	}

	got := tbl.String()
	if !strings.HasPrefix(got, "route add svc / http://10.0.0.1:80/ weight 0.0000") {
		t.Errorf("unexpected String output: %q", got)
	}
	if strings.Contains(got, "weight -") {
		t.Errorf("String output contains negative sign: %q", got)
	}

	// A target built directly with -0 must also normalize.
	r2 := &Route{Host: "", Path: "/"}
	r2.addTarget("svc", mustParse("http://10.0.0.1:80/"), negZero, nil, nil)
	if got := r2.Targets[0].FixedWeight; got != 0 {
		t.Errorf("addTarget(-0): got %v want 0", got)
	}
	if s := r2.TargetConfig(r2.Targets[0], false); strings.Contains(s, "-0") {
		t.Errorf("TargetConfig contains negative sign: %q", s)
	}
}

// TestStringRoundTripPreservesTargets verifies that Table.String() (used by
// the admin raw view) can be read back by NewTable and yields the same set of
// targets and fixed weights. This includes drained targets and dynamic
// targets which were assigned a share of 0.
func TestStringRoundTripPreservesTargets(t *testing.T) {
	in := `
route add svc / http://bar:111/
route add svc / http://bar:222/ weight 0.2500
route add svc / http://bar:333/ weight 0.7500
`
	tbl, err := NewTable(bytes.NewBufferString(in))
	if err != nil {
		t.Fatal(err)
	}

	want := []struct {
		url    string
		weight float64
	}{
		{"http://bar:111/", -1},
		{"http://bar:222/", 0.25},
		{"http://bar:333/", 0.75},
	}

	check := func(desc string, tbl Table) {
		t.Helper()
		r := tbl[""][0]
		if len(r.Targets) != len(want) {
			t.Fatalf("%s: got %d targets want %d: %q", desc, len(r.Targets), len(want), tbl.String())
		}
		for _, w := range want {
			tg := findTarget(r, w.url)
			if tg == nil {
				t.Fatalf("%s: missing target %s: %q", desc, w.url, tbl.String())
			}
			if tg.FixedWeight != w.weight {
				t.Errorf("%s: %s FixedWeight got %v want %v", desc, w.url, tg.FixedWeight, w.weight)
			}
		}
	}

	check("original", tbl)

	s := tbl.String()
	rt, err := NewTable(bytes.NewBufferString(s))
	if err != nil {
		t.Fatalf("re-parse failed: %v\n%s", err, s)
	}
	check("re-parsed", rt)

	for _, w := range want {
		if !strings.Contains(s, w.url) {
			t.Errorf("String does not contain %s: %q", w.url, s)
		}
	}
}

// TestConfigTrueUnchanged verifies that the normalized config view keeps
// dropping drained targets and dynamic targets with a 0 share.
func TestConfigTrueUnchanged(t *testing.T) {
	// dynamic target with a 0 share
	tbl, err := NewTable(bytes.NewBufferString(`
route add svc / http://bar:111/
route add svc / http://bar:222/ weight 0.5
route add svc / http://bar:333/ weight 0.5
`))
	if err != nil {
		t.Fatal(err)
	}
	got := tbl.config(true)
	want := []string{
		`route add svc / http://bar:222/ weight 0.5000`,
		`route add svc / http://bar:333/ weight 0.5000`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// drained targets are dropped
	tbl, err = NewTable(bytes.NewBufferString(`
route add a foo.com/ http://a:80/ weight 0
route add b foo.com/ http://b:80/
`))
	if err != nil {
		t.Fatal(err)
	}
	got = tbl.config(true)
	want = []string{`route add b foo.com/ http://b:80/ weight 1.0000`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestPickersEmptyRing verifies that the pickers do not panic when the
// weighted ring is empty.
func TestPickersEmptyRing(t *testing.T) {
	r := &Route{Host: "foo.com", Path: "/"}
	r.addTarget("svc", mustParse("http://a:80/"), 0, nil, nil)
	r.addTarget("svc", mustParse("http://b:80/"), 0, nil, nil)

	if got, want := len(r.wTargets), 0; got != want {
		t.Fatalf("wTargets: got %d want %d", got, want)
	}

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("picker panicked: %v", rec)
		}
	}()

	prev := randIntn
	defer func() { randIntn = prev }()
	randIntn = func(int) int { return 0 }

	if got := rndPicker(r); got != nil {
		t.Errorf("rndPicker: got %v want nil", got)
	}
	if got := rrPicker(r); got != nil {
		t.Errorf("rrPicker: got %v want nil", got)
	}
}

// TestWeighTargetsOnlyDrained verifies the internal ring is empty and that no
// target keeps a positive weight when all fixed weights are 0.
func TestWeighTargetsOnlyDrained(t *testing.T) {
	r := &Route{Host: "foo.com", Path: "/"}
	r.addTarget("svc", mustParse("http://a:80/"), 0, nil, nil)
	r.addTarget("svc", mustParse("http://b:80/"), 0, nil, nil)

	if len(r.wTargets) != 0 {
		t.Fatalf("wTargets: got %d want 0", len(r.wTargets))
	}
	for _, tg := range r.Targets {
		if tg.Weight != 0 {
			t.Errorf("%s: got weight %v want 0", tg.URL, tg.Weight)
		}
	}
}

// TestSetWeightZeroDrains verifies that "route weight svc / weight 0" drains
// the matching targets and that the route then has no target.
func TestSetWeightZeroDrains(t *testing.T) {
	tbl, err := NewTable(bytes.NewBufferString(`
route add svc foo.com/ http://a:80/
route add svc foo.com/ http://b:80/
route weight svc foo.com/ weight 0
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []picker{rrPicker, rndPicker} {
		if tg := mustLookup(t, tbl, "foo.com", "/x", p); tg != nil {
			t.Errorf("%T: got %v want nil", p, tg)
		}
	}
}

// TestRRPickerConcurrent verifies that the round-robin picker does not race
// and distributes evenly when called concurrently.
func TestRRPickerConcurrent(t *testing.T) {
	r := &Route{Host: "foo.com", Path: "/"}
	r.addTarget("svc", mustParse("http://a:80/"), -1, nil, nil)
	r.addTarget("svc", mustParse("http://b:80/"), -1, nil, nil)

	const (
		goroutines = 8
		lookups    = 1000
	)
	var a, b atomic.Int64
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range lookups {
				switch got := rrPicker(r).URL.String(); got {
				case "http://a:80/":
					a.Add(1)
				case "http://b:80/":
					b.Add(1)
				}
			}
		})
	}
	wg.Wait()

	if got, want := a.Load(), int64(goroutines*lookups/2); got != want {
		t.Errorf("a: got %d want %d", got, want)
	}
	if got, want := b.Load(), int64(goroutines*lookups/2); got != want {
		t.Errorf("b: got %d want %d", got, want)
	}
}

// TestLookupConcurrentTableReplace exercises Lookup concurrently while the
// active routing table is replaced. The tables are T1 (50/50), T2 (both
// drained) and T3 (50/50 again). The goroutines race on a start barrier and
// each performs 1000 lookups. T2 must yield nil without a panic for both
// pickers and T3 must distribute evenly with the rr picker. The metric series
// are not touched so that this test also passes under -race without running
// cleanup against the shared counters.
func TestLookupConcurrentTableReplace(t *testing.T) {
	build := func(weight string) Table {
		t.Helper()
		tbl, err := NewTable(bytes.NewBufferString(`
route add a foo.com/ http://a:80/ ` + weight + `
route add b foo.com/ http://b:80/ ` + weight + `
`))
		if err != nil {
			t.Fatalf("build(%q): %v", weight, err)
		}
		return tbl
	}

	t1 := build("weight 0.5")
	t2 := build("weight 0")
	t3 := build("weight 0.5")

	SetTable(t1)
	defer SetTable(make(Table))

	const (
		goroutines = 8
		lookups    = 1000
		total      = int64(goroutines * lookups)
	)

	for _, tc := range []struct {
		desc    string
		table   Table
		pick    picker
		drained bool
	}{
		{desc: "T1 rr", table: t1, pick: rrPicker},
		{desc: "T2 rr drained", table: t2, pick: rrPicker, drained: true},
		{desc: "T3 rr resumes", table: t3, pick: rrPicker},
		{desc: "T2 rnd drained", table: t2, pick: rndPicker, drained: true},
	} {
		SetTable(tc.table)

		prev := randIntn
		randIntn = func(int) int { return 0 }

		var mu sync.Mutex
		counts := map[string]int64{}
		var panicked atomic.Bool
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range goroutines {
			wg.Go(func() {
				local := map[string]int64{}
				func() {
					defer func() {
						if r := recover(); r != nil {
							panicked.Store(true)
						}
					}()
					<-start
					req := &http.Request{Host: "foo.com", URL: mustParse("/x")}
					for range lookups {
						if tg := GetTable().Lookup(req, tc.pick, prefixMatcher, globCache, globDisabled); tg == nil {
							local["nil"]++
						} else {
							local[tg.URL.String()]++
						}
					}
				}()
				mu.Lock()
				for k, v := range local {
					counts[k] += v
				}
				mu.Unlock()
			})
		}
		close(start)
		wg.Wait()
		randIntn = prev

		if panicked.Load() {
			t.Fatalf("%s: Lookup panicked", tc.desc)
		}

		if tc.drained {
			if got := counts["nil"]; got != total {
				t.Errorf("%s: got %d nil want %d (counts %v)", tc.desc, got, total, counts)
			}
			continue
		}
		if got, want := counts["http://a:80/"], total/2; got != want {
			t.Errorf("%s: a got %d want %d (counts %v)", tc.desc, got, want, counts)
		}
		if got, want := counts["http://b:80/"], total/2; got != want {
			t.Errorf("%s: b got %d want %d (counts %v)", tc.desc, got, want, counts)
		}
		if counts["nil"] != 0 {
			t.Errorf("%s: got %d nil want 0", tc.desc, counts["nil"])
		}
	}
}

// TestDrainedTargetsKeepMetrics verifies that replacing the routing table with
// a table in which all targets are drained does not delete their metric
// series and that the counters keep accumulating when traffic resumes.
// It reuses the testDeletableCounter from metrics_cleanup_test.go to count
// DeleteLabelValues calls.
func TestDrainedTargetsKeepMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	rxCv := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "test",
		Name:      "route_rx_total",
		Help:      "test",
	}, []string{"service", "host", "path", "target"})
	txCv := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "test",
		Name:      "route_tx_total",
		Help:      "test",
	}, []string{"service", "host", "path", "target"})
	reg.MustRegister(rxCv, txCv)

	var deletes atomic.Int64
	rx := &countingDeletableCounter{testDeletableCounter: &testDeletableCounter{cv: rxCv}, deletes: &deletes}
	tx := &countingDeletableCounter{testDeletableCounter: &testDeletableCounter{cv: txCv}, deletes: &deletes}

	prevRx, prevTx := counters.rxCounter, counters.txCounter
	defer func() { counters.rxCounter, counters.txCounter = prevRx, prevTx }()
	counters.rxCounter = rx
	counters.txCounter = tx

	mustTable := func(s string) Table {
		t.Helper()
		tbl, err := NewTable(bytes.NewBufferString(s))
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		return tbl
	}
	t1 := mustTable("route add a foo.com/ http://a:80/ weight 0.5\nroute add b foo.com/ http://b:80/ weight 0.5")
	t2 := mustTable("route add a foo.com/ http://a:80/ weight 0\nroute add b foo.com/ http://b:80/ weight 0")
	t3 := mustTable("route add a foo.com/ http://a:80/ weight 0.5\nroute add b foo.com/ http://b:80/ weight 0.5")

	// traffic and series exist under T1
	for _, tg := range t1["foo.com"][0].Targets {
		tg.RxCounter.Add(1)
	}

	// T2 drains all targets, T3 restores them. Neither must delete the series.
	SetTable(t1)
	SetTable(t2)
	SetTable(t3)

	if got := deletes.Load(); got != 0 {
		t.Errorf("DeleteLabelValues called %d times want 0", got)
	}

	// The series of target a survived T2 and keeps accumulating under T3.
	target := findTarget(t3["foo.com"][0], "http://a:80/")
	target.RxCounter.Add(1)

	cv := rxCv.WithLabelValues("a", "foo.com", "/", "http://a:80/")
	if got, want := testutil.ToFloat64(cv), float64(2); got != want {
		t.Errorf("rx counter for a: got %v want %v", got, want)
	}
	if got, want := testutil.ToFloat64(rxCv.WithLabelValues("b", "foo.com", "/", "http://b:80/")), float64(1); got != want {
		t.Errorf("rx counter for b: got %v want %v", got, want)
	}
}

// countingDeletableCounter counts calls to DeleteLabelValues and delegates to
// the prometheus backed helper from metrics_cleanup_test.go.
type countingDeletableCounter struct {
	*testDeletableCounter
	deletes *atomic.Int64
}

func (c *countingDeletableCounter) DeleteLabelValues(labelValues ...string) bool {
	c.deletes.Add(1)
	return c.testDeletableCounter.DeleteLabelValues(labelValues...)
}

var _ metrics.DeletableCounter = (*countingDeletableCounter)(nil)

// TestDeletedVsDrainedDifferentFromNoMatch verifies the distinction between a
// route whose targets were deleted and one whose targets were drained.
// A deleted route no longer matches and falls through to the next matching
// route. A drained route matches but is terminal.
func TestDeletedVsDrainedDifferentFromNoMatch(t *testing.T) {
	const deleted = `
route add svca / http://foo.com:800
route add svcb /foo http://foo.com:900
route del svcb /foo`
	const drained = `
route add svca / http://foo.com:800
route add svcb /foo http://foo.com:900 weight 0`

	tblDeleted, err := NewTable(bytes.NewBufferString(deleted))
	if err != nil {
		t.Fatal(err)
	}
	if tg := mustLookup(t, tblDeleted, "", "/foo", rrPicker); tg == nil || tg.URL.String() != "http://foo.com:800" {
		t.Errorf("deleted: got %v want http://foo.com:800", tg)
	}

	tblDrained, err := NewTable(bytes.NewBufferString(drained))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []picker{rrPicker, rndPicker} {
		if tg := mustLookup(t, tblDrained, "", "/foo", p); tg != nil {
			t.Errorf("drained %T: got %v want nil", p, tg)
		}
	}
}

// TestLookupHostMatched verifies the matched flag used by the dynamic TCP
// lookup to stop probing further hosts when a host matched but was drained.
func TestLookupHostMatched(t *testing.T) {
	tbl, err := NewTable(bytes.NewBufferString(`
route add a foo.com/ http://a:80/ weight 0
route add b bar.com/ http://b:80/
`))
	if err != nil {
		t.Fatal(err)
	}

	if tg, matched := tbl.LookupHostMatched("foo.com", rrPicker); !matched || tg != nil {
		t.Errorf("foo.com: got (%v, %v) want (nil, true)", tg, matched)
	}
	if tg, matched := tbl.LookupHostMatched("bar.com", rrPicker); !matched || tg == nil {
		t.Errorf("bar.com: got (%v, %v) want (target, true)", tg, matched)
	}
	if tg, matched := tbl.LookupHostMatched("unknown.com", rrPicker); matched || tg != nil {
		t.Errorf("unknown.com: got (%v, %v) want (nil, false)", tg, matched)
	}
}

// TestRouteDefUnmarshalWeight verifies the custom backend JSON decoding:
// an omitted weight is dynamic (-1), an explicit 0 drains, an explicit value
// is taken as-is, and a route weight command without a weight is invalid.
func TestRouteDefUnmarshalWeight(t *testing.T) {
	const (
		addNoWeight   = `{"cmd":"route add","service":"s","src":"foo.com/","dst":"http://10.0.0.1:80/"}`
		addWeight0    = `{"cmd":"route add","service":"s","src":"foo.com/","dst":"http://10.0.0.1:80/","weight":0}`
		addWeight050  = `{"cmd":"route add","service":"s","src":"foo.com/","dst":"http://10.0.0.1:80/","weight":0.5}`
		customMissing = `{"cmd":"route weight","service":"s","src":"foo.com/"}`
	)
	tests := []struct {
		desc  string
		in    string
		check func(float64) bool
		msg   string
	}{
		{"add no weight", addNoWeight, func(w float64) bool { return w == -1 }, "-1"},
		{"add weight 0", addWeight0, func(w float64) bool { return w == 0 }, "0"},
		{"add weight 0.5", addWeight050, func(w float64) bool { return w == 0.5 }, "0.5"},
		{"weight cmd missing weight", customMissing, func(w float64) bool { return w != w }, "NaN"},
	}
	for _, tt := range tests {
		var d RouteDef
		if err := json.Unmarshal([]byte(tt.in), &d); err != nil {
			t.Fatalf("%s: %v", tt.desc, err)
		}
		if !tt.check(d.Weight) {
			t.Errorf("%s: got weight %v want %s", tt.desc, d.Weight, tt.msg)
		}
	}

	// marshalling keeps the existing shape including the weight field
	d := RouteDef{Cmd: RouteAddCmd, Service: "s", Src: "foo.com/", Dst: "http://10.0.0.1:80/", Weight: -1}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["weight"]; !ok {
		t.Errorf("marshalled RouteDef misses weight: %s", b)
	}
}

// TestCustomBackendNoWeightRouting verifies the end-to-end behavior of the
// custom backend for a route add with and without a weight field.
func TestCustomBackendNoWeightRouting(t *testing.T) {
	const noWeight = `[{"cmd":"route add","service":"s","src":"foo.com/","dst":"http://10.0.0.1:80/"}]`
	const weight0 = `[{"cmd":"route add","service":"s","src":"foo.com/","dst":"http://10.0.0.1:80/","weight":0}]`

	for _, tt := range []struct {
		desc string
		in   string
		want string // empty means no target
	}{
		{"omitted weight", noWeight, "http://10.0.0.1:80/"},
		{"explicit 0", weight0, ""},
	} {
		var defs []RouteDef
		if err := json.Unmarshal([]byte(tt.in), &defs); err != nil {
			t.Fatalf("%s: %v", tt.desc, err)
		}
		tbl, err := NewTableCustom(&defs)
		if err != nil {
			t.Fatalf("%s: %v", tt.desc, err)
		}
		tg := mustLookup(t, tbl, "foo.com", "/x", rrPicker)
		if tt.want == "" {
			if tg != nil {
				t.Errorf("%s: got %v want nil", tt.desc, tg)
			}
			continue
		}
		if tg == nil || tg.URL.String() != tt.want {
			t.Errorf("%s: got %v want %v", tt.desc, tg, tt.want)
		}
	}
}

// TestCustomBackendWeightCmdMissingWeight verifies that a route weight command
// without a weight field is ignored instead of draining all matching targets.
func TestCustomBackendWeightCmdMissingWeight(t *testing.T) {
	const in = `[
{"cmd":"route add","service":"s","src":"foo.com/","dst":"http://10.0.0.1:80/"},
{"cmd":"route add","service":"s","src":"foo.com/","dst":"http://10.0.0.2:80/"},
{"cmd":"route weight","service":"s","src":"foo.com/"}
]`
	var defs []RouteDef
	if err := json.Unmarshal([]byte(in), &defs); err != nil {
		t.Fatal(err)
	}
	tbl, err := NewTableCustom(&defs)
	if err != nil {
		t.Fatal(err)
	}
	tg := mustLookup(t, tbl, "foo.com", "/x", rrPicker)
	if tg == nil {
		t.Fatal("got nil want target: invalid route weight command drained the targets")
	}
	r := tbl["foo.com"][0]
	for _, target := range r.Targets {
		if target.FixedWeight != -1 {
			t.Errorf("%s: got FixedWeight %v want -1", target.URL, target.FixedWeight)
		}
	}
}
