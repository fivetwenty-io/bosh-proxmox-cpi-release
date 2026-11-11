package http

import (
	"net/http"
	"slices"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/constants"
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
//
// The url field of a log line and of a hook event holds the scheme, host, and
// path of the request, without user information or a fragment. It also holds
// the query when LogQueryParams is set, with the value of each parameter named
// in RedactParams replaced, as the query field shows it. RedactHeaders and
// RedactParams match without regard to case, and the credential headers and
// parameters the SDK itself sends or receives, such as Authorization, Cookie,
// Set-Cookie, ticket, and vncticket, are redacted whatever the lists say.
//
// LogResponseBody logs a body that parses as JSON with the value of each
// secret member replaced, such as ticket, CSRFPreventionToken, password, and
// the value of a newly created token. A body that does not parse is not
// logged. Its length and content type are logged in its place. LogBody is
// reserved: the client does not log request bodies.
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
		RedactHeaders:     []string{headerAuthorization, headerCookie, headerCSRF},
		RedactParams:      []string{redactParamPassword, redactParamToken, redactParamSecret},
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

// redact returns a new map with sensitive keys redacted. A key matches a name
// in redactKeys, or one in alwaysRedacted, without regard to case.
func redact(in map[string][]string, redactKeys []string, alwaysRedacted ...string) map[string]interface{} {
	names := newNameMatcher(redactKeys, alwaysRedacted)
	out := make(map[string]interface{}, len(in))

	for key, vals := range in {
		if names.secret(key) {
			out[key] = redactedValue
		} else {
			// copy values
			cp := make([]string, len(vals))
			copy(cp, vals)
			out[key] = cp
		}
	}

	return out
}

// logState is the logger, logging configuration, and hook list requests read.
// A published logState is never modified: SetLogger, SetLogConfig, and AddHook
// install a changed copy, so each logging call, such as one request log line
// or one round of hooks, reads one consistent set without a lock while those
// setters run. The separate calls a request makes may see different sets when
// a setter runs during the request.
type logState struct {
	logger Logger
	config LogConfig
	hooks  []Hook
}

// noLogging is the logState of a Client built without NewClient and before any
// logging setter runs: no logger, logging disabled, and no hooks.
var noLogging = &logState{} //nolint:gochecknoglobals // read-only zero value shared by every such Client

// active reports whether the state has a logger and logging is enabled.
func (s *logState) active() bool {
	return s.logger != nil && s.config.Enabled
}

// cloneLogConfig returns cfg with its slices copied, so neither the caller nor
// the client can change the other's copy through a shared backing array.
func cloneLogConfig(cfg LogConfig) LogConfig {
	cfg.RedactHeaders = slices.Clone(cfg.RedactHeaders)
	cfg.RedactParams = slices.Clone(cfg.RedactParams)

	return cfg
}

// logging returns the logState in effect, for reading only.
func (c *Client) logging() *logState {
	if state := c.logs.Load(); state != nil {
		return state
	}

	return noLogging
}

// updateLogging installs a copy of the current logState changed by apply.
// logMu serializes the copy and the store, so concurrent setters never lose
// each other's change.
func (c *Client) updateLogging(apply func(next *logState)) {
	c.logMu.Lock()
	defer c.logMu.Unlock()

	next := *c.logging()
	apply(&next)
	c.logs.Store(&next)
}

// SetLogger installs the structured logger for subsequent requests. It is safe
// to call while requests are in flight.
func (c *Client) SetLogger(l Logger) {
	c.updateLogging(func(next *logState) { next.logger = l })
}

// SetLogConfig installs the logging configuration for subsequent requests. The
// client keeps its own copy of the redaction lists. It is safe to call while
// requests are in flight.
func (c *Client) SetLogConfig(cfg LogConfig) {
	cfg = cloneLogConfig(cfg)

	c.updateLogging(func(next *logState) { next.config = cfg })
}

// AddHook appends a hook that receives an event after each request. Requests
// already in flight may not call it. It is safe to call while requests are in
// flight.
func (c *Client) AddHook(h Hook) {
	c.updateLogging(func(next *logState) {
		// Clip forces append to copy, leaving the published list untouched.
		next.hooks = append(slices.Clip(next.hooks), h)
	})
}

// LogConfig returns a copy of the current logging configuration, including
// its redaction lists, so changing it does not change the client's.
func (c *Client) LogConfig() LogConfig { return cloneLogConfig(c.logging().config) }

// logWarn logs msg at Warn when a logger is set and logging is enabled.
func (c *Client) logWarn(msg string, fields map[string]interface{}) {
	logs := c.logging()
	if !logs.active() {
		return
	}

	logs.logger.Warn(msg, fields)
}

// logRequest logs the request metadata according to config.
func (c *Client) logRequest(req *http.Request, msg string, extra map[string]interface{}) {
	logs := c.logging()
	if !logs.active() {
		return
	}

	fields := map[string]interface{}{
		logFieldMethod: req.Method,
		logFieldURL:    redactedURL(req.URL, logs.config),
	}

	for k, v := range extra {
		fields[k] = v
	}

	if logs.config.LogRequestHeader {
		fields["headers"] = redact(req.Header, logs.config.RedactHeaders, alwaysRedactedHeaders()...)
	}

	if logs.config.LogQueryParams {
		fields["query"] = redact(req.URL.Query(), logs.config.RedactParams, alwaysRedactedParams()...)
	}

	logs.logger.Info(msg, fields)
}

func (c *Client) fireHook(event *Event) {
	logs := c.logging()

	for _, hook := range logs.hooks {
		// Hooks are best-effort and must not panic or block. Recover so one
		// misbehaving hook cannot crash the request path, but surface the panic
		// at Warn instead of swallowing it silently.
		func(hook Hook) {
			defer func() {
				if r := recover(); r != nil && logs.logger != nil {
					logs.logger.Warn("log hook panicked", map[string]interface{}{"panic": r})
				}
			}()

			hook(event)
		}(hook)
	}
}
