package service

import (
	"context"
	"sync"
)

// UpstreamRequestBodyStats counts bodies handed to the HTTP transport, not
// acknowledged network bytes. RawBytes is the uncompressed baseline per attempt.
// Only the official Codex OAuth HTTP route is measured; no payload is retained.
type UpstreamRequestBodyStats struct {
	RawBytes           int64  `json:"raw_bytes"`
	EncodedBytes       int64  `json:"encoded_bytes"`
	Attempts           int    `json:"attempts"`
	CompressedAttempts int    `json:"compressed_attempts"`
	Fallbacks          int    `json:"fallbacks"`
	Encoding           string `json:"encoding"`
	Reason             string `json:"reason"`
	FinalRawBytes      int64  `json:"final_raw_bytes"`
	FinalEncodedBytes  int64  `json:"final_encoded_bytes"`
}

type upstreamBodyStatsKey struct{}
type upstreamBodyStatsCollector struct {
	mu    sync.Mutex
	stats UpstreamRequestBodyStats
}

func WithUpstreamRequestBodyStats(ctx context.Context) context.Context {
	return context.WithValue(ctx, upstreamBodyStatsKey{}, &upstreamBodyStatsCollector{})
}

// CopyUpstreamRequestBodyStats freezes request-scoped counters for async billing.
func CopyUpstreamRequestBodyStats(parent, base context.Context) context.Context {
	if stats := upstreamRequestBodyStats(parent); stats != nil {
		return context.WithValue(base, upstreamBodyStatsKey{}, &upstreamBodyStatsCollector{stats: *stats})
	}
	return base
}

func upstreamRequestBodyStats(ctx context.Context) *UpstreamRequestBodyStats {
	c, _ := ctx.Value(upstreamBodyStatsKey{}).(*upstreamBodyStatsCollector)
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stats.Attempts == 0 {
		return nil
	}
	stats := c.stats
	return &stats
}

func recordUpstreamRequestBody(ctx context.Context, raw, encoded int64, encoding, reason string, fallback bool) {
	c, _ := ctx.Value(upstreamBodyStatsKey{}).(*upstreamBodyStatsCollector)
	if c == nil || raw < 0 || encoded < 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.RawBytes += raw
	c.stats.EncodedBytes += encoded
	c.stats.Attempts++
	if encoding == "zstd" {
		c.stats.CompressedAttempts++
	}
	if fallback {
		c.stats.Fallbacks++
	}
	c.stats.Encoding, c.stats.Reason = encoding, reason
	c.stats.FinalRawBytes, c.stats.FinalEncodedBytes = raw, encoded
}
