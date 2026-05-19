package constants

import "time"

// HTTP status codes.
const (
	HTTPStatusBadRequest          = 400
	HTTPStatusUnauthorized        = 401
	HTTPStatusForbidden           = 403
	HTTPStatusNotFound            = 404
	HTTPStatusInternalServerError = 500
)

// Network and timeout values.
const (
	ProxmoxDefaultPort      = 8006
	DefaultTimeoutSeconds   = 45
	DefaultKeepAliveSeconds = 15
	ShortTimeoutSeconds     = 10
	MediumTimeoutSeconds    = 60
	LongTimeoutSeconds      = 90
)

// Task timeouts in seconds.
const (
	DefaultTaskTimeoutSeconds = 300
	MediumTaskTimeoutSeconds  = 600
	LongTaskTimeoutSeconds    = 1200
	TaskIntervalMillis        = 1000
	MaxTaskIntervalMillis     = 5000
)

// Buffer and memory sizes.
const (
	DefaultBufferSize = 1024
	LargeBufferSize   = 4096
	DefaultMemoryMB   = 1024
)

// File permissions.
const (
	DirPermissions  = 0700
	FilePermissions = 0600
)

// Batch processing.
const (
	DefaultMaxBatchSize   = 100
	DefaultMaxConcurrency = 10
)

// VM defaults for examples.
const (
	DefaultTemplateVMID = 9000
	DefaultVMID         = 10000
	DefaultNewVMID      = 10001
)

// Parsing and validation.
const (
	ExpectedPartsCount  = 2
	ExpectedMatchCount  = 3
	MinimumMatchCount   = 3
	SHA256ByteLength    = 32
	TicketValidityHours = 2
)

// Retry and jitter.
const (
	DefaultMaxRetries = 3
	BackoffMultiplier = 2
	JitterPercentage  = 100
)

// Size conversions.
const (
	BytesPerKB = 1024
	BytesPerMB = BytesPerKB * 1024
	BytesPerGB = BytesPerMB * 1024
)

// Additional timeout values.
const (
	BatchTimeoutMinutes               = 5
	DefaultClientTimeoutSeconds       = 30
	MaxIdleTimeoutSeconds             = 600
	WebSocketHandshakeTimeoutSeconds  = 30
	StreamTickerSeconds               = 10
	WebSocketReconnectIntervalSeconds = 5
	WebSocketMaxReconnectAttempts     = 10
	SummaryMaxAgeMinutes              = 10
)

// Channel and buffer sizes.
const (
	ErrorChannelSize      = 10
	StreamMaxItemSize     = 1024 * 1024 // 1MB
	MillisecondsPerSecond = 1000
	AttributeMultiplier   = 2
)

// Pool configuration.
const (
	MaxConnections             = 100
	MaxConnectionsPerHost      = 10
	FailureRateThreshold       = 0.5
	AverageResponseTimeDivisor = 2
)

// Time duration functions for convenience.
func ShortTimeout() time.Duration              { return ShortTimeoutSeconds * time.Second }
func MediumTimeout() time.Duration             { return MediumTimeoutSeconds * time.Second }
func LongTimeout() time.Duration               { return LongTimeoutSeconds * time.Second }
func DefaultTimeout() time.Duration            { return DefaultTimeoutSeconds * time.Second }
func DefaultClientTimeout() time.Duration      { return DefaultClientTimeoutSeconds * time.Second }
func BatchTimeout() time.Duration              { return BatchTimeoutMinutes * time.Minute }
func MaxIdleTimeout() time.Duration            { return MaxIdleTimeoutSeconds * time.Second }
func WebSocketHandshakeTimeout() time.Duration { return WebSocketHandshakeTimeoutSeconds * time.Second }
func StreamTickerDuration() time.Duration      { return StreamTickerSeconds * time.Second }
func WebSocketReconnectInterval() time.Duration {
	return WebSocketReconnectIntervalSeconds * time.Second
}
func SummaryMaxAge() time.Duration  { return SummaryMaxAgeMinutes * time.Minute }
func TicketValidity() time.Duration { return TicketValidityHours * time.Hour }
