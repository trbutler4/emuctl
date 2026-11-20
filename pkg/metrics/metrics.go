// Package metrics is a small Prometheus text-format registry, so emuctl keeps
// its standard-library-only build. It supports labeled counters, labeled
// histograms and gauges computed at scrape time.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Registry struct {
	mu      sync.Mutex
	metrics []metric
}

type metric interface {
	write(w io.Writer)
}

func NewRegistry() *Registry { return &Registry{} }

// series key: label values joined by \xff.
func key(vals []string) string { return strings.Join(vals, "\xff") }

func labelPairs(names, vals []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%s", n, strconv.Quote(vals[i]))
	}
	for i := 0; i+1 < len(extra); i += 2 {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%s", extra[i], strconv.Quote(extra[i+1]))
	}
	b.WriteByte('}')
	return b.String()
}

func fmtFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Counter is a monotonically increasing labeled counter.
type Counter struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	vals       map[string]float64
	lv         map[string][]string
}

func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	c := &Counter{name: name, help: help, labels: labels, vals: map[string]float64{}, lv: map[string][]string{}}
	r.mu.Lock()
	r.metrics = append(r.metrics, c)
	r.mu.Unlock()
	return c
}

// Add increments the series for label values (in declaration order).
func (c *Counter) Add(v float64, labelValues ...string) {
	if len(labelValues) != len(c.labels) {
		panic(fmt.Sprintf("%s: want %d label values, got %d", c.name, len(c.labels), len(labelValues)))
	}
	k := key(labelValues)
	c.mu.Lock()
	c.vals[k] += v
	c.lv[k] = labelValues
	c.mu.Unlock()
}

func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Value returns a series value, for tests.
func (c *Counter) Value(labelValues ...string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vals[key(labelValues)]
}

func (c *Counter) write(w io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	for _, k := range sortedKeys(c.vals) {
		fmt.Fprintf(w, "%s%s %s\n", c.name, labelPairs(c.labels, c.lv[k]), fmtFloat(c.vals[k]))
	}
}

// Histogram is a labeled histogram with fixed buckets.
type Histogram struct {
	name, help string
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*hseries
}

type hseries struct {
	lv     []string
	counts []uint64 // per bucket, non-cumulative
	count  uint64
	sum    float64
}

func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	h := &Histogram{name: name, help: help, labels: labels, buckets: b, series: map[string]*hseries{}}
	r.mu.Lock()
	r.metrics = append(r.metrics, h)
	r.mu.Unlock()
	return h
}

func (h *Histogram) Observe(v float64, labelValues ...string) {
	if len(labelValues) != len(h.labels) {
		panic(fmt.Sprintf("%s: want %d label values, got %d", h.name, len(h.labels), len(labelValues)))
	}
	k := key(labelValues)
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.series[k]
	if s == nil {
		s = &hseries{lv: labelValues, counts: make([]uint64, len(h.buckets))}
		h.series[k] = s
	}
	for i, ub := range h.buckets {
		if v <= ub {
			s.counts[i]++
			break
		}
	}
	s.count++
	s.sum += v
}

// Count returns how many observations a series has, for tests.
func (h *Histogram) Count(labelValues ...string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.series[key(labelValues)]; s != nil {
		return s.count
	}
	return 0
}

func (h *Histogram) write(w io.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := h.series[k]
		var cum uint64
		for i, ub := range h.buckets {
			cum += s.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelPairs(h.labels, s.lv, "le", fmtFloat(ub)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelPairs(h.labels, s.lv, "le", "+Inf"), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.name, labelPairs(h.labels, s.lv), fmtFloat(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.name, labelPairs(h.labels, s.lv), s.count)
	}
}

// GaugeFunc registers a gauge family computed on every scrape. f calls set
// once per series with label values in declaration order.
func (r *Registry) GaugeFunc(name, help string, labels []string, f func(set func(v float64, labelValues ...string))) {
	r.mu.Lock()
	r.metrics = append(r.metrics, &gaugeFunc{typ: "gauge", name: name, help: help, labels: labels, f: f})
	r.mu.Unlock()
}

// CounterFunc registers a counter family read on every scrape, for counts
// another package already keeps.
func (r *Registry) CounterFunc(name, help string, labels []string, f func(set func(v float64, labelValues ...string))) {
	r.mu.Lock()
	r.metrics = append(r.metrics, &gaugeFunc{typ: "counter", name: name, help: help, labels: labels, f: f})
	r.mu.Unlock()
}

type gaugeFunc struct {
	typ, name, help string
	labels          []string
	f               func(set func(v float64, labelValues ...string))
}

func (g *gaugeFunc) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", g.name, g.help, g.name, g.typ)
	type row struct {
		lv []string
		v  float64
	}
	var rows []row
	g.f(func(v float64, lv ...string) {
		if len(lv) != len(g.labels) {
			panic(fmt.Sprintf("%s: want %d label values, got %d", g.name, len(g.labels), len(lv)))
		}
		rows = append(rows, row{append([]string(nil), lv...), v})
	})
	sort.Slice(rows, func(i, j int) bool { return key(rows[i].lv) < key(rows[j].lv) })
	for _, r := range rows {
		fmt.Fprintf(w, "%s%s %s\n", g.name, labelPairs(g.labels, r.lv), fmtFloat(r.v))
	}
}

func sortedKeys(m map[string]float64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Write writes every metric in Prometheus text exposition format 0.0.4.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	ms := append([]metric(nil), r.metrics...)
	r.mu.Unlock()
	for _, m := range ms {
		m.write(w)
	}
}

// Handler serves the registry.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}

// Common bucket sets, in seconds.
var (
	// FastBuckets fit API requests and adb calls.
	FastBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	// SlowBuckets fit boots, installs, queue waits and lease lengths.
	SlowBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400}
)
