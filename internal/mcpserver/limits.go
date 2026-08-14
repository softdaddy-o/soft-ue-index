package mcpserver

import "time"

const (
	defaultMaxItems = 100
	defaultMaxQuery = 512
	defaultMaxPath  = 4096
	defaultMaxBytes = 32 * 1024
)

// Limits bound every MCP request and response so a malformed client cannot
// turn code intelligence into an unbounded local-file or memory operation.
type Limits struct {
	MaxItems       int
	MaxQueryBytes  int
	MaxPathBytes   int
	MaxSourceBytes int
	Timeout        time.Duration
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
	if l.Timeout <= 0 {
		l.Timeout = 15 * time.Second
	}
	return l
}
