package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/internal/constants"
)

// Logger is a minimal structured logger interface.
// Implementations may wrap zap, zerolog, etc.
type Logger interface {
	Debug(msg string, fields map[string]interface{})
	Info(msg string, fields map[string]interface{})
	Warn(msg string, fields map[string]interface{})
	Error(msg string, fields map[string]interface{})
}

// LogConfig controls logging behavior and redaction.
type LogConfig struct {
	Enabled           bool
	RedactHeaders     []string
	RedactParams      []string
	LogRequestHeader  bool
	LogQueryParams    bool
	LogBody           bool
	LogResponseHeader bool
	LogResponseBody   bool
	MaxBodyBytes      int
	SampleRate        float64 // 0..1, not used yet (placeholder)
}

func defaultLogConfig() LogConfig {
	return LogConfig{
		Enabled:           true,
		RedactHeaders:     []string{"authorization", "cookie", "csrfpreventiontoken"},
		RedactParams:      []string{"password", "token", "secret"},
		LogRequestHeader:  false,
		LogResponseHeader: false,
		LogQueryParams:    true,
		LogBody:           false,
		LogResponseBody:   false,
		MaxBodyBytes:      constants.DefaultBufferSize,
		SampleRate:        1.0,
	}
}

// Hook receives request/response lifecycle events.
type Hook func(event *Event)

// Event represents a request/response log event.
type Event struct {
	Method   string
	URL      string
	Status   int
	Duration time.Duration
	Err      error
	Fields   map[string]interface{}
}

// redact returns a new map with sensitive keys redacted.
func redact(in map[string][]string, redactKeys []string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for key, vals := range in {
		lowerKey := strings.ToLower(key)
		redacted := false

		for _, rk := range redactKeys {
			if lowerKey == rk {
				redacted = true

				break
			}
		}

		if redacted {
			out[key] = "REDACTED"
		} else {
			// copy values
			cp := make([]string, len(vals))
			copy(cp, vals)
			out[key] = cp
		}
	}

	return out
}

func (c *Client) SetLogger(l Logger)         { c.logger = l }
func (c *Client) SetLogConfig(cfg LogConfig) { c.logConfig = cfg }
func (c *Client) AddHook(h Hook)             { c.hooks = append(c.hooks, h) }

// LogConfig returns the current logging configuration.
func (c *Client) LogConfig() LogConfig { return c.logConfig }

// logRequest logs the request metadata according to config.
func (c *Client) logRequest(req *http.Request, msg string, extra map[string]interface{}) {
	if c.logger == nil || !c.logConfig.Enabled {
		return
	}

	fields := map[string]interface{}{
		"method": req.Method,
		"url":    req.URL.String(),
	}

	for k, v := range extra {
		fields[k] = v
	}

	if c.logConfig.LogRequestHeader {
		fields["headers"] = redact(req.Header, c.logConfig.RedactHeaders)
	}

	if c.logConfig.LogQueryParams {
		fields["query"] = redact(req.URL.Query(), c.logConfig.RedactParams)
	}

	c.logger.Info(msg, fields)
}

func (c *Client) fireHook(ev *Event) {
	for _, h := range c.hooks {
		// hooks are best-effort and should not panic or block
		func(h Hook) { defer func() { _ = recover() }(); h(ev) }(h)
	}
}
