package proxy

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

type webSocketTimingKey struct{}

// Local call boundaries, not claims about when the remote server received bytes.
// This observation is owned by the request goroutine and read after openUpstream.
// Handshake wait sums all attempts; dial timestamps span the first actual dial
// through the last completed attempt, including any retry backoff between them.
type webSocketTiming struct {
	handshakeWaitObserved bool
	handshakeWait         time.Duration
	dialStart, dialDone   time.Time
	handshakeStatus       int
	writeStart, writeDone time.Time
}

func webSocketTimingFromContext(ctx context.Context) *webSocketTiming {
	if observed, _ := ctx.Value(webSocketTimingKey{}).(*webSocketTiming); observed != nil {
		return observed
	}
	return &webSocketTiming{}
}

func (o *webSocketTiming) writeHeaders(h http.Header) {
	if o.handshakeWaitObserved {
		h.Set("X-BPS-Handshake-Rate-Wait-Ms", strconv.FormatInt(o.handshakeWait.Milliseconds(), 10))
	}
	for name, at := range map[string]time.Time{
		"X-BPS-Ws-Dial-Start-Unix-Ms":         o.dialStart,
		"X-BPS-Ws-Dial-Done-Unix-Ms":          o.dialDone,
		"X-BPS-Ws-Create-Write-Start-Unix-Ms": o.writeStart,
		"X-BPS-Ws-Create-Write-Done-Unix-Ms":  o.writeDone,
	} {
		if !at.IsZero() {
			h.Set(name, strconv.FormatInt(at.UnixMilli(), 10))
		}
	}
	if o.handshakeStatus != 0 {
		h.Set("X-BPS-Ws-Handshake-Status", strconv.Itoa(o.handshakeStatus))
	}
}
