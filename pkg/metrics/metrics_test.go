package metrics

import (
	"strings"
	"sync"
	"testing"
)

func render(r *Registry) string {
	var b strings.Builder
	r.Write(&b)
	return b.String()
}

func TestExposition(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("x_total", "Things.", "device", "result")
	c.Inc("p1", "ok")
	c.Add(2, "p1", "ok")
	c.Inc("p2", `we"ird`)
	h := r.Histogram("d_seconds", "Durations.", []float64{1, 5}, "device")
	h.Observe(0.5, "p1")
	h.Observe(3, "p1")
	h.Observe(10, "p1")
	r.GaugeFunc("g", "Gauge.", nil, func(set func(float64, ...string)) { set(7) })
	got := render(r)
	want := `# HELP x_total Things.
# TYPE x_total counter
x_total{device="p1",result="ok"} 3
x_total{device="p2",result="we\"ird"} 1
# HELP d_seconds Durations.
# TYPE d_seconds histogram
d_seconds_bucket{device="p1",le="1"} 1
d_seconds_bucket{device="p1",le="5"} 2
d_seconds_bucket{device="p1",le="+Inf"} 3
d_seconds_sum{device="p1"} 13.5
d_seconds_count{device="p1"} 3
# HELP g Gauge.
# TYPE g gauge
g 7
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestConcurrentUse(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("c_total", "C.", "k")
	h := r.Histogram("h_seconds", "H.", FastBuckets, "k")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.Inc("a")
				h.Observe(0.1, "a")
				if j%100 == 0 {
					render(r)
				}
			}
		}()
	}
	wg.Wait()
	if c.Value("a") != 8000 || h.Count("a") != 8000 {
		t.Fatalf("lost updates: %v %v", c.Value("a"), h.Count("a"))
	}
}

func TestWrongLabelCountPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic")
		}
	}()
	NewRegistry().Counter("c", "C.", "a", "b").Inc("only-one")
}
