package mcpserver

import "time"

const (
	defaultMaxItems = 100
	defaultMaxQuery = 512
	defaultMaxPath  = 4096
	defaultMaxBytes = 32 * 1024
	// minimumResponseBytes leaves room for the SDK's newline-delimited JSON-RPC
	// envelope as well as a useful structured tool result.
	minimumResponseBytes      = 512
	protocolEnvelopeBytes     = 256
	defaultMaxRequestBytes    = 64 * 1024
	defaultMaxRequestIDBytes  = 128
	defaultMaxConcurrentTools = 8
	maxRequestIDTokenBytes    = protocolEnvelopeBytes - 128
	// indexReadyBudgetMultiplier scales Limits.Timeout into the outer context
	// budget for search_symbols, the one tool that can block on a cold
	// clangd background index. It stays proportional to the configured
	// Timeout (rather than a fixed duration) so a caller who tightens
	// Timeout for tests or low-latency deployments gets a proportionally
	// tighter bound here too.
	indexReadyBudgetMultiplier = 3
)

// Limits bound every MCP request and response so a malformed client cannot
// turn code intelligence into an unbounded local-file or memory operation.
type Limits struct {
	MaxItems         int
	MaxQueryBytes    int
	MaxPathBytes     int
	MaxSourceBytes   int
	MaxResponseBytes int
	MaxRequestBytes  int
	// MaxRequestIDBytes limits the raw JSON token bytes of a string request ID,
	// including quotes and escapes, before the SDK can echo it in a response.
	MaxRequestIDBytes  int
	MaxCallDepth       int
	MaxConcurrentTools int
	Timeout            time.Duration
}

func (l Limits) normalized() Limits {
	if l.MaxItems <= 0 {
		l.MaxItems = defaultMaxItems
	}
	if l.MaxQueryBytes <= 0 {
		l.MaxQueryBytes = defaultMaxQuery
	}
	if l.MaxPathBytes <= 0 {
		l.MaxPathBytes = defaultMaxPath
	}
	if l.MaxSourceBytes <= 0 {
		l.MaxSourceBytes = defaultMaxBytes
	}
	if l.MaxResponseBytes <= 0 {
		l.MaxResponseBytes = defaultMaxBytes
	}
	if l.MaxRequestBytes <= 0 {
		l.MaxRequestBytes = defaultMaxRequestBytes
	}
	if l.MaxRequestIDBytes <= 0 {
		l.MaxRequestIDBytes = defaultMaxRequestIDBytes
	}
	if l.MaxCallDepth <= 0 {
		l.MaxCallDepth = 3
	}
	if l.MaxConcurrentTools <= 0 {
		l.MaxConcurrentTools = defaultMaxConcurrentTools
	}
	if l.Timeout <= 0 {
		l.Timeout = 30 * time.Second
	}
	return l
}

func (l Limits) validate() error {
	if l.MaxResponseBytes > 0 && l.MaxResponseBytes < minimumResponseBytes {
		return ErrInvalidLimits
	}
	if (l.MaxRequestBytes > 0 && l.MaxRequestBytes < 2) || (l.MaxRequestIDBytes > 0 && (l.MaxRequestIDBytes < 1 || l.MaxRequestIDBytes > maxRequestIDTokenBytes)) {
		return ErrInvalidLimits
	}
	return nil
}
