package metrics

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultLatencyBuckets defines standard latency histogram boundaries (in seconds)
// tailored for storage engine and network operations, spanning 100 microseconds (0.0001s)
// up to 10 seconds.
var DefaultLatencyBuckets = []float64{
	0.0001,  // 100 µs
	0.00025, // 250 µs
	0.0005,  // 500 µs
	0.001,   // 1 ms
	0.0025,  // 2.5 ms
	0.005,   // 5 ms
	0.01,    // 10 ms
	0.025,   // 25 ms
	0.05,    // 50 ms
	0.1,     // 100 ms
	0.25,    // 250 ms
	0.5,     // 500 ms
	1.0,     // 1 s
	2.5,     // 2.5 s
	5.0,     // 5 s
	10.0,    // 10 s
}

// DefaultCompactionBuckets defines standard duration histogram boundaries (in seconds)
// tailored for background compaction and flush jobs, spanning 50 milliseconds up to 5 minutes.
var DefaultCompactionBuckets = []float64{
	0.05,  // 50 ms
	0.1,   // 100 ms
	0.25,  // 250 ms
	0.5,   // 500 ms
	1.0,   // 1 s
	2.5,   // 2.5 s
	5.0,   // 5 s
	10.0,  // 10 s
	30.0,  // 30 s
	60.0,  // 1 min
	120.0, // 2 min
	300.0, // 5 min
}

// MetricType identifies the Prometheus metric type.
type MetricType string

const (
	TypeCounter   MetricType = "counter"
	TypeGauge     MetricType = "gauge"
	TypeHistogram MetricType = "histogram"
)

// Label represents a key-value label pair.
type Label struct {
	Name  string
	Value string
}

// Counter is a monotonically increasing 64-bit unsigned integer counter.
type Counter struct {
	val atomic.Uint64
}

// NewCounter constructs an initialized Counter.
func NewCounter() *Counter {
	return &Counter{}
}

// Inc increments the counter by 1.
func (c *Counter) Inc() {
	c.val.Add(1)
}

// Add adds the given non-negative delta to the counter.
func (c *Counter) Add(delta uint64) {
	c.val.Add(delta)
}

// Value returns the current counter value.
func (c *Counter) Value() uint64 {
	return c.val.Load()
}

// Gauge is a 64-bit signed integer value that can arbitrarily increase and decrease.
type Gauge struct {
	val atomic.Int64
}

// NewGauge constructs an initialized Gauge.
func NewGauge() *Gauge {
	return &Gauge{}
}

// Set sets the gauge to the specified value.
func (g *Gauge) Set(val int64) {
	g.val.Store(val)
}

// Add adds delta to the gauge.
func (g *Gauge) Add(delta int64) {
	g.val.Add(delta)
}

// Sub subtracts delta from the gauge.
func (g *Gauge) Sub(delta int64) {
	g.val.Add(-delta)
}

// Value returns the current gauge value.
func (g *Gauge) Value() int64 {
	return g.val.Load()
}

// Histogram tracks the statistical distribution of observations across fixed bucket boundaries.
// Observations are recorded lock-free with zero heap allocations on the hot path.
type Histogram struct {
	boundaries []float64        // Upper boundaries (strictly ascending, does not include +Inf)
	buckets    []atomic.Uint64  // Discrete observation counts per bucket; len = len(boundaries) + 1 (last is overflow)
	count      atomic.Uint64    // Total number of observations
	sumNs      atomic.Int64     // Cumulative sum of observed durations in nanoseconds
}

// NewHistogram constructs a Histogram with the specified bucket boundaries.
// Boundaries must be strictly positive and strictly ascending.
func NewHistogram(boundaries []float64) *Histogram {
	b := make([]float64, len(boundaries))
	copy(b, boundaries)
	sort.Float64s(b)

	return &Histogram{
		boundaries: b,
		buckets:    make([]atomic.Uint64, len(b)+1),
	}
}

// Observe records a float64 observation (in seconds).
func (h *Histogram) Observe(seconds float64) {
	if seconds < 0 || math.IsNaN(seconds) {
		seconds = 0
	}
	h.observeNs(seconds, int64(seconds*1e9))
}

// ObserveDuration records an observation from a time.Duration.
func (h *Histogram) ObserveDuration(d time.Duration) {
	if d < 0 {
		d = 0
	}
	h.observeNs(d.Seconds(), d.Nanoseconds())
}

func (h *Histogram) observeNs(seconds float64, ns int64) {
	idx := sort.Search(len(h.boundaries), func(i int) bool {
		return h.boundaries[i] >= seconds
	})
	h.buckets[idx].Add(1)
	h.count.Add(1)
	if ns > 0 {
		h.sumNs.Add(ns)
	}
}

// HistogramSnapshot contains a point-in-time consistent view of histogram state.
type HistogramSnapshot struct {
	Count       uint64
	SumSeconds  float64
	Boundaries  []float64
	Cumulative  []uint64 // Cumulative counts corresponding to Boundaries; final entry is +Inf
}

// Snapshot returns a point-in-time calculation of cumulative bucket counts.
func (h *Histogram) Snapshot() HistogramSnapshot {
	totalCount := h.count.Load()
	sumNs := h.sumNs.Load()
	sumSec := float64(sumNs) / 1e9

	discrete := make([]uint64, len(h.buckets))
	for i := range h.buckets {
		discrete[i] = h.buckets[i].Load()
	}

	cumulative := make([]uint64, len(h.boundaries)+1)
	var running uint64
	for i := 0; i < len(h.boundaries); i++ {
		running += discrete[i]
		cumulative[i] = running
	}
	// The +Inf bucket captures all observations
	running += discrete[len(h.boundaries)]
	// If concurrent observations occurred while reading discrete buckets, clamp to max(running, totalCount)
	if totalCount > running {
		running = totalCount
	}
	cumulative[len(h.boundaries)] = running

	return HistogramSnapshot{
		Count:      running,
		SumSeconds: sumSec,
		Boundaries: h.boundaries,
		Cumulative: cumulative,
	}
}

// HistogramVec provides a partition of Histograms by pre-defined discrete label combinations.
// All label keys and their permitted discrete values are declared at initialization,
// guaranteeing strict O(1) bounded cardinality and zero dynamic map expansion.
type HistogramVec struct {
	boundaries   []float64
	labelNames   []string
	labelValues  []string
	entries      map[string]*Histogram
	orderedKeys  []string
}

// NewHistogramVec constructs a HistogramVec with strictly bounded label values.
func NewHistogramVec(boundaries []float64, labelNames []string, allowedValues map[string][]string) *HistogramVec {
	if len(labelNames) == 0 {
		panic("metrics: HistogramVec requires at least one label name")
	}

	// Generate Cartesian product of allowed label values
	var combinations [][]Label
	var generateCombos func(nameIdx int, current []Label)
	generateCombos = func(nameIdx int, current []Label) {
		if nameIdx == len(labelNames) {
			comboCopy := make([]Label, len(current))
			copy(comboCopy, current)
			combinations = append(combinations, comboCopy)
			return
		}
		name := labelNames[nameIdx]
		vals := allowedValues[name]
		if len(vals) == 0 {
			panic(fmt.Sprintf("metrics: label %q must have at least one allowed value", name))
		}
		for _, val := range vals {
			generateCombos(nameIdx+1, append(current, Label{Name: name, Value: val}))
		}
	}
	generateCombos(0, nil)

	entries := make(map[string]*Histogram, len(combinations))
	var orderedKeys []string
	for _, combo := range combinations {
		key := formatLabelsKey(combo)
		entries[key] = NewHistogram(boundaries)
		orderedKeys = append(orderedKeys, key)
	}
	sort.Strings(orderedKeys)

	return &HistogramVec{
		boundaries:  boundaries,
		labelNames:  labelNames,
		entries:     entries,
		orderedKeys: orderedKeys,
	}
}

// WithLabelValues returns the Histogram corresponding to the given label values.
// Returns a no-op fallback Histogram if the values were not in the pre-registered allowed set,
// strictly defending against cardinality explosion from unexpected inputs.
func (hv *HistogramVec) WithLabelValues(vals ...string) *Histogram {
	if len(vals) != len(hv.labelNames) {
		return noopHistogram
	}
	var labels []Label
	for i, name := range hv.labelNames {
		labels = append(labels, Label{Name: name, Value: vals[i]})
	}
	key := formatLabelsKey(labels)
	if h, ok := hv.entries[key]; ok {
		return h
	}
	return noopHistogram
}

var noopHistogram = NewHistogram([]float64{1.0})

// CounterVec provides a partition of Counters by pre-defined discrete label combinations.
type CounterVec struct {
	labelNames  []string
	entries     map[string]*Counter
	orderedKeys []string
}

// NewCounterVec constructs a CounterVec with strictly bounded label values.
func NewCounterVec(labelNames []string, allowedValues map[string][]string) *CounterVec {
	if len(labelNames) == 0 {
		panic("metrics: CounterVec requires at least one label name")
	}

	var combinations [][]Label
	var generateCombos func(nameIdx int, current []Label)
	generateCombos = func(nameIdx int, current []Label) {
		if nameIdx == len(labelNames) {
			comboCopy := make([]Label, len(current))
			copy(comboCopy, current)
			combinations = append(combinations, comboCopy)
			return
		}
		name := labelNames[nameIdx]
		vals := allowedValues[name]
		if len(vals) == 0 {
			panic(fmt.Sprintf("metrics: label %q must have at least one allowed value", name))
		}
		for _, val := range vals {
			generateCombos(nameIdx+1, append(current, Label{Name: name, Value: val}))
		}
	}
	generateCombos(0, nil)

	entries := make(map[string]*Counter, len(combinations))
	var orderedKeys []string
	for _, combo := range combinations {
		key := formatLabelsKey(combo)
		entries[key] = NewCounter()
		orderedKeys = append(orderedKeys, key)
	}
	sort.Strings(orderedKeys)

	return &CounterVec{
		labelNames:  labelNames,
		entries:     entries,
		orderedKeys: orderedKeys,
	}
}

// WithLabelValues returns the Counter for the specified label values, or a no-op Counter if unrecognized.
func (cv *CounterVec) WithLabelValues(vals ...string) *Counter {
	if len(vals) != len(cv.labelNames) {
		return noopCounter
	}
	var labels []Label
	for i, name := range cv.labelNames {
		labels = append(labels, Label{Name: name, Value: vals[i]})
	}
	key := formatLabelsKey(labels)
	if c, ok := cv.entries[key]; ok {
		return c
	}
	return noopCounter
}

var noopCounter = NewCounter()

func formatLabelsKey(labels []Label) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Name)
		b.WriteByte('=')
		b.WriteString(l.Value)
	}
	return b.String()
}

func parseLabelsFromKey(key string) []Label {
	if key == "" {
		return nil
	}
	parts := strings.Split(key, ",")
	labels := make([]Label, 0, len(parts))
	for _, p := range parts {
		if idx := strings.IndexByte(p, '='); idx != -1 {
			labels = append(labels, Label{Name: p[:idx], Value: p[idx+1:]})
		}
	}
	return labels
}

// GaugeFunc is an evaluated callback for dynamically computing a gauge at scrape time.
type GaugeFunc func() int64

type registeredCounter struct {
	help string
	c    *Counter
}

type registeredCounterVec struct {
	help string
	cv   *CounterVec
}

type registeredGauge struct {
	help string
	g    *Gauge
}

type registeredGaugeFunc struct {
	help   string
	labels []Label
	fn     GaugeFunc
}

type registeredHistogram struct {
	help string
	h    *Histogram
}

type registeredHistogramVec struct {
	help string
	hv   *HistogramVec
}

// Registry manages and exposes an isolated collection of metrics.
type Registry struct {
	mu            sync.RWMutex
	counters      map[string]*registeredCounter
	counterVecs   map[string]*registeredCounterVec
	gauges        map[string]*registeredGauge
	gaugeFuncs    map[string][]*registeredGaugeFunc
	histograms    map[string]*registeredHistogram
	histogramVecs map[string]*registeredHistogramVec
}

// NewRegistry constructs an empty, isolated metrics Registry.
func NewRegistry() *Registry {
	return &Registry{
		counters:      make(map[string]*registeredCounter),
		counterVecs:   make(map[string]*registeredCounterVec),
		gauges:        make(map[string]*registeredGauge),
		gaugeFuncs:    make(map[string][]*registeredGaugeFunc),
		histograms:    make(map[string]*registeredHistogram),
		histogramVecs: make(map[string]*registeredHistogramVec),
	}
}

// RegisterCounter registers a simple Counter.
func (r *Registry) RegisterCounter(name, help string, c *Counter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[name] = &registeredCounter{help: help, c: c}
}

// RegisterCounterVec registers a CounterVec.
func (r *Registry) RegisterCounterVec(name, help string, cv *CounterVec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counterVecs[name] = &registeredCounterVec{help: help, cv: cv}
}

// RegisterGauge registers a simple Gauge.
func (r *Registry) RegisterGauge(name, help string, g *Gauge) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[name] = &registeredGauge{help: help, g: g}
}

// RegisterGaugeFunc registers an evaluated callback gauge.
func (r *Registry) RegisterGaugeFunc(name, help string, labels []Label, fn GaugeFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.gaugeFuncs[name]
	key := formatLabelsKey(labels)
	for i, entry := range list {
		if formatLabelsKey(entry.labels) == key {
			list[i] = &registeredGaugeFunc{help: help, labels: labels, fn: fn}
			return
		}
	}
	r.gaugeFuncs[name] = append(list, &registeredGaugeFunc{help: help, labels: labels, fn: fn})
}

// UnregisterGaugeFunc removes any registered GaugeFunc entries under the given name.
func (r *Registry) UnregisterGaugeFunc(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.gaugeFuncs, name)
}

// RegisterHistogram registers a simple Histogram.
func (r *Registry) RegisterHistogram(name, help string, h *Histogram) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.histograms[name] = &registeredHistogram{help: help, h: h}
}

// RegisterHistogramVec registers a HistogramVec.
func (r *Registry) RegisterHistogramVec(name, help string, hv *HistogramVec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.histogramVecs[name] = &registeredHistogramVec{help: help, hv: hv}
}

// EscapeLabelValue escapes string characters for Prometheus exposition format.
// Replaces \ with \\, " with \", and newline with \n.
func EscapeLabelValue(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// EscapeHelpText escapes help docstrings for Prometheus exposition format.
func EscapeHelpText(s string) string {
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func formatLabels(labels []Label, extraKey, extraVal string) string {
	if len(labels) == 0 && extraKey == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for _, l := range labels {
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(l.Name)
		b.WriteString(`="`)
		b.WriteString(EscapeLabelValue(l.Value))
		b.WriteByte('"')
	}
	if extraKey != "" {
		if !first {
			b.WriteByte(',')
		}
		b.WriteString(extraKey)
		b.WriteString(`="`)
		b.WriteString(EscapeLabelValue(extraVal))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// WritePrometheus writes all registered metrics in standard Prometheus exposition format 0.0.4.
func (r *Registry) WritePrometheus(w io.Writer) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var buf bytes.Buffer

	// 1. Counters
	var counterNames []string
	for name := range r.counters {
		counterNames = append(counterNames, name)
	}
	sort.Strings(counterNames)
	for _, name := range counterNames {
		rc := r.counters[name]
		fmt.Fprintf(&buf, "# HELP %s %s\n", name, EscapeHelpText(rc.help))
		fmt.Fprintf(&buf, "# TYPE %s counter\n", name)
		fmt.Fprintf(&buf, "%s %d\n", name, rc.c.Value())
	}

	// 2. CounterVecs
	var counterVecNames []string
	for name := range r.counterVecs {
		counterVecNames = append(counterVecNames, name)
	}
	sort.Strings(counterVecNames)
	for _, name := range counterVecNames {
		rcv := r.counterVecs[name]
		fmt.Fprintf(&buf, "# HELP %s %s\n", name, EscapeHelpText(rcv.help))
		fmt.Fprintf(&buf, "# TYPE %s counter\n", name)
		for _, key := range rcv.cv.orderedKeys {
			c := rcv.cv.entries[key]
			labels := parseLabelsFromKey(key)
			fmt.Fprintf(&buf, "%s%s %d\n", name, formatLabels(labels, "", ""), c.Value())
		}
	}

	// 3. Gauges
	var gaugeNames []string
	for name := range r.gauges {
		gaugeNames = append(gaugeNames, name)
	}
	sort.Strings(gaugeNames)
	for _, name := range gaugeNames {
		rg := r.gauges[name]
		fmt.Fprintf(&buf, "# HELP %s %s\n", name, EscapeHelpText(rg.help))
		fmt.Fprintf(&buf, "# TYPE %s gauge\n", name)
		fmt.Fprintf(&buf, "%s %d\n", name, rg.g.Value())
	}

	// 4. GaugeFuncs
	var gaugeFuncNames []string
	for name := range r.gaugeFuncs {
		gaugeFuncNames = append(gaugeFuncNames, name)
	}
	sort.Strings(gaugeFuncNames)
	for _, name := range gaugeFuncNames {
		funcs := r.gaugeFuncs[name]
		if len(funcs) == 0 {
			continue
		}
		fmt.Fprintf(&buf, "# HELP %s %s\n", name, EscapeHelpText(funcs[0].help))
		fmt.Fprintf(&buf, "# TYPE %s gauge\n", name)
		for _, rgf := range funcs {
			val := rgf.fn()
			fmt.Fprintf(&buf, "%s%s %d\n", name, formatLabels(rgf.labels, "", ""), val)
		}
	}

	// 5. Histograms
	var histNames []string
	for name := range r.histograms {
		histNames = append(histNames, name)
	}
	sort.Strings(histNames)
	for _, name := range histNames {
		rh := r.histograms[name]
		snap := rh.h.Snapshot()
		fmt.Fprintf(&buf, "# HELP %s %s\n", name, EscapeHelpText(rh.help))
		fmt.Fprintf(&buf, "# TYPE %s histogram\n", name)
		for i, bound := range snap.Boundaries {
			boundStr := strconv.FormatFloat(bound, 'g', -1, 64)
			fmt.Fprintf(&buf, "%s_bucket%s %d\n", name, formatLabels(nil, "le", boundStr), snap.Cumulative[i])
		}
		fmt.Fprintf(&buf, "%s_bucket%s %d\n", name, formatLabels(nil, "le", "+Inf"), snap.Cumulative[len(snap.Boundaries)])
		fmt.Fprintf(&buf, "%s_sum %s\n", name, strconv.FormatFloat(snap.SumSeconds, 'f', -1, 64))
		fmt.Fprintf(&buf, "%s_count %d\n", name, snap.Count)
	}

	// 6. HistogramVecs
	var histVecNames []string
	for name := range r.histogramVecs {
		histVecNames = append(histVecNames, name)
	}
	sort.Strings(histVecNames)
	for _, name := range histVecNames {
		rhv := r.histogramVecs[name]
		fmt.Fprintf(&buf, "# HELP %s %s\n", name, EscapeHelpText(rhv.help))
		fmt.Fprintf(&buf, "# TYPE %s histogram\n", name)
		for _, key := range rhv.hv.orderedKeys {
			h := rhv.hv.entries[key]
			labels := parseLabelsFromKey(key)
			snap := h.Snapshot()
			for i, bound := range snap.Boundaries {
				boundStr := strconv.FormatFloat(bound, 'g', -1, 64)
				fmt.Fprintf(&buf, "%s_bucket%s %d\n", name, formatLabels(labels, "le", boundStr), snap.Cumulative[i])
			}
			fmt.Fprintf(&buf, "%s_bucket%s %d\n", name, formatLabels(labels, "le", "+Inf"), snap.Cumulative[len(snap.Boundaries)])
			fmt.Fprintf(&buf, "%s_sum%s %s\n", name, formatLabels(labels, "", ""), strconv.FormatFloat(snap.SumSeconds, 'f', -1, 64))
			fmt.Fprintf(&buf, "%s_count%s %d\n", name, formatLabels(labels, "", ""), snap.Count)
		}
	}

	_, err := buf.WriteTo(w)
	return err
}
