// Package metrics provides Prometheus-compatible metrics for the PVE API client.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/internal/constants"
)

// Collector collects metrics for the PVE API client.
type Collector struct {
	counters   map[string]*Counter
	gauges     map[string]*Gauge
	histograms map[string]*Histogram
	summaries  map[string]*Summary
	mu         sync.RWMutex
	prefix     string
	labels     map[string]string
}

// NewCollector creates a new metrics collector.
func NewCollector(prefix string) *Collector {
	return &Collector{
		counters:   make(map[string]*Counter),
		gauges:     make(map[string]*Gauge),
		histograms: make(map[string]*Histogram),
		summaries:  make(map[string]*Summary),
		prefix:     prefix,
		labels:     make(map[string]string),
	}
}

// SetLabels sets global labels for all metrics.
func (c *Collector) SetLabels(labels map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.labels = labels
}

// Counter is a monotonically increasing value.
type Counter struct {
	name   string
	help   string
	value  int64
	labels map[string]string
}

// Inc increments the counter by 1.
func (co *Counter) Inc() {
	atomic.AddInt64(&co.value, 1)
}

// Add adds the given value to the counter.
func (co *Counter) Add(v int64) {
	atomic.AddInt64(&co.value, v)
}

// Get returns the current value.
func (co *Counter) Get() int64 {
	return atomic.LoadInt64(&co.value)
}

// NewCounter creates or gets a counter metric.
func (c *Collector) NewCounter(name, help string) *Counter {
	c.mu.Lock()
	defer c.mu.Unlock()

	fullName := c.prefix + "_" + name
	if counter, exists := c.counters[fullName]; exists {
		return counter
	}

	counter := &Counter{
		name:   fullName,
		help:   help,
		labels: make(map[string]string),
	}
	c.counters[fullName] = counter

	return counter
}

// Gauge is a value that can go up and down.
type Gauge struct {
	name   string
	help   string
	value  int64
	labels map[string]string
}

// Set sets the gauge to the given value.
func (g *Gauge) Set(v int64) {
	atomic.StoreInt64(&g.value, v)
}

// Inc increments the gauge by 1.
func (g *Gauge) Inc() {
	atomic.AddInt64(&g.value, 1)
}

// Dec decrements the gauge by 1.
func (g *Gauge) Dec() {
	atomic.AddInt64(&g.value, -1)
}

// Add adds the given value to the gauge.
func (g *Gauge) Add(v int64) {
	atomic.AddInt64(&g.value, v)
}

// Get returns the current value.
func (g *Gauge) Get() int64 {
	return atomic.LoadInt64(&g.value)
}

// NewGauge creates or gets a gauge metric.
func (c *Collector) NewGauge(name, help string) *Gauge {
	c.mu.Lock()
	defer c.mu.Unlock()

	fullName := c.prefix + "_" + name
	if gauge, exists := c.gauges[fullName]; exists {
		return gauge
	}

	gauge := &Gauge{
		name:   fullName,
		help:   help,
		labels: make(map[string]string),
	}
	c.gauges[fullName] = gauge

	return gauge
}

// Histogram tracks the distribution of values.
type Histogram struct {
	name    string
	help    string
	buckets []float64
	counts  []int64
	sum     int64
	count   int64
	mu      sync.RWMutex
	labels  map[string]string
}

// Observe adds a value to the histogram.
func (h *Histogram) Observe(value float64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Find the right bucket
	for i, bucket := range h.buckets {
		if value <= bucket {
			atomic.AddInt64(&h.counts[i], 1)

			break
		}
	}

	// Update sum and count
	atomic.AddInt64(&h.sum, int64(value*constants.MillisecondsPerSecond)) // Store as milliseconds
	atomic.AddInt64(&h.count, 1)
}

// GetStats returns histogram statistics.
func (h *Histogram) GetStats() (int64, float64, map[float64]int64) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	count := atomic.LoadInt64(&h.count)
	sum := float64(atomic.LoadInt64(&h.sum)) / float64(constants.MillisecondsPerSecond)

	buckets := make(map[float64]int64)
	for i, bucket := range h.buckets {
		buckets[bucket] = atomic.LoadInt64(&h.counts[i])
	}

	return count, sum, buckets
}

// NewHistogram creates or gets a histogram metric.
func (c *Collector) NewHistogram(name, help string, buckets []float64) *Histogram {
	c.mu.Lock()
	defer c.mu.Unlock()

	fullName := c.prefix + "_" + name
	if histogram, exists := c.histograms[fullName]; exists {
		return histogram
	}

	if buckets == nil {
		// Default buckets (in seconds)
		buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	}

	histogram := &Histogram{
		name:    fullName,
		help:    help,
		buckets: buckets,
		counts:  make([]int64, len(buckets)),
		labels:  make(map[string]string),
	}
	c.histograms[fullName] = histogram

	return histogram
}

// Summary tracks the distribution of values with quantiles.
type Summary struct {
	name       string
	help       string
	values     []float64
	maxAge     time.Duration
	timestamps []time.Time
	mu         sync.RWMutex
	labels     map[string]string
}

// Observe adds a value to the summary.
func (s *Summary) Observe(value float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	s.values = append(s.values, value)
	s.timestamps = append(s.timestamps, now)

	// Clean old values
	s.cleanOld(now)
}

// GetQuantiles returns the quantiles.
func (s *Summary) GetQuantiles(quantiles []float64) map[float64]float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.values) == 0 {
		result := make(map[float64]float64)
		for _, q := range quantiles {
			result[q] = 0
		}

		return result
	}

	// Copy and sort values
	sorted := make([]float64, len(s.values))
	copy(sorted, s.values)
	sort.Float64s(sorted)

	result := make(map[float64]float64)

	for _, q := range quantiles {
		index := int(float64(len(sorted)-1) * q)
		result[q] = sorted[index]
	}

	return result
}

// NewSummary creates or gets a summary metric.
func (c *Collector) NewSummary(name, help string, maxAge time.Duration) *Summary {
	c.mu.Lock()
	defer c.mu.Unlock()

	fullName := c.prefix + "_" + name
	if summary, exists := c.summaries[fullName]; exists {
		return summary
	}

	if maxAge == 0 {
		maxAge = constants.SummaryMaxAge()
	}

	summary := &Summary{
		name:       fullName,
		help:       help,
		maxAge:     maxAge,
		values:     make([]float64, 0),
		timestamps: make([]time.Time, 0),
		labels:     make(map[string]string),
	}
	c.summaries[fullName] = summary

	return summary
}

// Export exports metrics in Prometheus format.
func (c *Collector) Export(writer io.Writer) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	err := c.exportCounters(writer)
	if err != nil {
		return err
	}

	err = c.exportGauges(writer)
	if err != nil {
		return err
	}

	err = c.exportHistograms(writer)
	if err != nil {
		return err
	}

	err = c.exportSummaries(writer)
	if err != nil {
		return err
	}

	return nil
}

// Reset resets all metrics.
func (c *Collector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, counter := range c.counters {
		atomic.StoreInt64(&counter.value, 0)
	}

	for _, gauge := range c.gauges {
		atomic.StoreInt64(&gauge.value, 0)
	}

	for _, histogram := range c.histograms {
		histogram.mu.Lock()

		for i := range histogram.counts {
			atomic.StoreInt64(&histogram.counts[i], 0)
		}

		atomic.StoreInt64(&histogram.sum, 0)
		atomic.StoreInt64(&histogram.count, 0)
		histogram.mu.Unlock()
	}

	for _, summary := range c.summaries {
		summary.mu.Lock()
		summary.values = make([]float64, 0)
		summary.timestamps = make([]time.Time, 0)
		summary.mu.Unlock()
	}
}

func (s *Summary) cleanOld(now time.Time) {
	cutoff := now.Add(-s.maxAge)

	index := 0
	for index < len(s.timestamps) && s.timestamps[index].Before(cutoff) {
		index++
	}

	if index > 0 {
		s.values = s.values[index:]
		s.timestamps = s.timestamps[index:]
	}
}

func (c *Collector) exportCounters(writer io.Writer) error {
	for name, counter := range c.counters {
		err := c.writeMetricHeader(writer, name, counter.help, "counter")
		if err != nil {
			return err
		}

		labels := c.formatLabels(counter.labels)

		_, err = fmt.Fprintf(writer, "%s%s %d\n", name, labels, counter.Get())
		if err != nil {
			return fmt.Errorf("failed to write counter metric: %w", err)
		}
	}

	return nil
}

func (c *Collector) exportGauges(writer io.Writer) error {
	for name, gauge := range c.gauges {
		err := c.writeMetricHeader(writer, name, gauge.help, "gauge")
		if err != nil {
			return err
		}

		labels := c.formatLabels(gauge.labels)

		_, err = fmt.Fprintf(writer, "%s%s %d\n", name, labels, gauge.Get())
		if err != nil {
			return fmt.Errorf("failed to write gauge metric: %w", err)
		}
	}

	return nil
}

func (c *Collector) exportHistograms(writer io.Writer) error {
	for name, histogram := range c.histograms {
		err := c.writeMetricHeader(writer, name, histogram.help, "histogram")
		if err != nil {
			return err
		}

		err = c.exportHistogramData(writer, name, histogram)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Collector) exportSummaries(writer io.Writer) error {
	for name, summary := range c.summaries {
		err := c.writeMetricHeader(writer, name, summary.help, "summary")
		if err != nil {
			return err
		}

		err = c.exportSummaryData(writer, name, summary)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Collector) writeMetricHeader(writer io.Writer, name, help, metricType string) error {
	_, err := fmt.Fprintf(writer, "# HELP %s %s\n", name, help)
	if err != nil {
		return fmt.Errorf("failed to write metric help header: %w", err)
	}

	_, err = fmt.Fprintf(writer, "# TYPE %s %s\n", name, metricType)
	if err != nil {
		return fmt.Errorf("failed to write metric type header: %w", err)
	}

	return nil
}

func (c *Collector) exportHistogramData(writer io.Writer, name string, histogram *Histogram) error {
	count, sum, buckets := histogram.GetStats()
	labels := c.formatLabels(histogram.labels)

	// Export buckets
	cumulative := int64(0)

	var err error

	for _, bucket := range histogram.buckets {
		cumulative += buckets[bucket]

		bucketLabel := c.formatBucketLabel(labels, bucket)

		_, err = fmt.Fprintf(writer, "%s_bucket%s %d\n", name, bucketLabel, cumulative)
		if err != nil {
			return fmt.Errorf("failed to write histogram bucket: %w", err)
		}
	}

	// Export +Inf bucket
	infLabel := c.formatBucketLabel(labels, -1) // Special case for +Inf

	_, err = fmt.Fprintf(writer, "%s_bucket%s %d\n", name, infLabel, count)
	if err != nil {
		return fmt.Errorf("failed to write histogram +Inf bucket: %w", err)
	}

	// Export sum and count
	_, err = fmt.Fprintf(writer, "%s_sum%s %.3f\n", name, labels, sum)
	if err != nil {
		return fmt.Errorf("failed to write histogram sum: %w", err)
	}

	_, err = fmt.Fprintf(writer, "%s_count%s %d\n", name, labels, count)
	if err != nil {
		return fmt.Errorf("failed to write histogram count: %w", err)
	}

	return nil
}

func (c *Collector) exportSummaryData(writer io.Writer, name string, summary *Summary) error {
	quantiles := summary.GetQuantiles([]float64{0.5, 0.9, 0.99})
	labels := c.formatLabels(summary.labels)

	var err error

	for q, value := range quantiles {
		quantileLabel := c.formatQuantileLabel(labels, q)

		_, err = fmt.Fprintf(writer, "%s%s %.3f\n", name, quantileLabel, value)
		if err != nil {
			return fmt.Errorf("failed to write summary quantile: %w", err)
		}
	}

	return nil
}

func (c *Collector) formatBucketLabel(labels string, bucket float64) string {
	if bucket == -1 { // +Inf case
		if labels == "" {
			return "{le=\"+Inf\"}"
		}

		return labels + ",le=\"+Inf\""
	}

	if labels == "" {
		return fmt.Sprintf("{le=\"%.3f\"}", bucket)
	}

	return fmt.Sprintf("%s,le=\"%.3f\"", labels, bucket)
}

func (c *Collector) formatQuantileLabel(labels string, quantile float64) string {
	if labels == "" {
		return fmt.Sprintf("{quantile=\"%.2f\"}", quantile)
	}

	return fmt.Sprintf("%s,quantile=\"%.2f\"", labels, quantile)
}

func (c *Collector) formatLabels(metricLabels map[string]string) string {
	if len(c.labels) == 0 && len(metricLabels) == 0 {
		return ""
	}

	// Merge labels
	merged := make(map[string]string)
	for k, v := range c.labels {
		merged[k] = v
	}

	for k, v := range metricLabels {
		merged[k] = v
	}

	// Format as Prometheus labels
	parts := make([]string, 0, len(merged))
	for k, v := range merged {
		parts = append(parts, fmt.Sprintf("%s=\"%s\"", k, v))
	}

	sort.Strings(parts)

	return "{" + strings.Join(parts, ",") + "}"
}

// DefaultMetrics provides default metrics for the PVE API client.
type DefaultMetrics struct {
	RequestsTotal       *Counter
	RequestsFailedTotal *Counter
	RequestDuration     *Histogram
	ActiveConnections   *Gauge
	BytesSent           *Counter
	BytesReceived       *Counter
	collector           *Collector
}

// NewDefaultMetrics creates default metrics for the PVE API client.
func NewDefaultMetrics() *DefaultMetrics {
	collector := NewCollector("pve_api_client")

	return &DefaultMetrics{
		RequestsTotal:       collector.NewCounter("requests_total", "Total number of API requests"),
		RequestsFailedTotal: collector.NewCounter("requests_failed_total", "Total number of failed API requests"),
		RequestDuration:     collector.NewHistogram("request_duration_seconds", "API request duration in seconds", nil),
		ActiveConnections:   collector.NewGauge("active_connections", "Number of active connections"),
		BytesSent:           collector.NewCounter("bytes_sent_total", "Total bytes sent"),
		BytesReceived:       collector.NewCounter("bytes_received_total", "Total bytes received"),
		collector:           collector,
	}
}

// Export exports the default metrics.
func (m *DefaultMetrics) Export(w io.Writer) error {
	return m.collector.Export(w)
}

// Reset resets all default metrics.
func (m *DefaultMetrics) Reset() {
	m.collector.Reset()
}
