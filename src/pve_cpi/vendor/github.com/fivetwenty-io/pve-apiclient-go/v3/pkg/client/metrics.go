package client

import (
	pvehttp "github.com/fivetwenty-io/pve-apiclient-go/v3/internal/http"
	"time"
)

// Metrics is a read-only snapshot of client metrics.
type Metrics struct {
	Requests      int64
	Errors        int64
	TotalDuration time.Duration
}

// MetricsOf returns a metrics snapshot from the underlying HTTP client if available.
func MetricsOf(c Client) (Metrics, bool) {
	// unwrap our private type
	impl, ok := c.(*client)
	if !ok {
		return Metrics{}, false
	}
	// reach into adapter
	if ia, ok := impl.httpClient.(*internalHTTPAdapter); ok && ia.inner != nil {
		m := ia.inner.Metrics()

		return Metrics{Requests: m.Requests, Errors: m.Errors, TotalDuration: m.TotalDuration}, true
	}
	// direct usage fallback
	if ihc, ok := any(impl.httpClient).(*pvehttp.Client); ok {
		m := ihc.Metrics()

		return Metrics{Requests: m.Requests, Errors: m.Errors, TotalDuration: m.TotalDuration}, true
	}

	return Metrics{}, false
}
