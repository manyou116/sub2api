package proxy

import (
	"net"
	"syscall"
	"testing"
)

func TestWebSocketHandshakeWindowsSocketErrors(t *testing.T) {
	for _, code := range []syscall.Errno{syscall.WSAECONNRESET, syscall.WSAECONNABORTED} {
		reason, retry := handshakeFailureKind(&net.OpError{Op: "read", Net: "tcp", Err: code})
		if !retry || reason != "connection_interrupted" {
			t.Fatalf("Winsock error %d not classified: %s retry=%t", code, reason, retry)
		}
	}
	reason, retry := handshakeFailureKind(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.Errno(10061)})
	if retry || reason != "connection_refused" {
		t.Fatalf("Winsock refusal: %s retry=%t", reason, retry)
	}
}
