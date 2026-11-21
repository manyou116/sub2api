package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type reuseScopeKey struct{}
type reuseScope struct {
	id     string
	reused atomic.Bool
}

// WithWebSocketReuse takes a scope produced by the authenticated gateway.
func WithWebSocketReuse(ctx context.Context, scope string) (context.Context, *atomic.Bool) {
	if scope == "" {
		return ctx, nil
	}
	s := &reuseScope{id: scope}
	return context.WithValue(ctx, reuseScopeKey{}, s), &s.reused
}

type reuseSlot struct {
	worker *startRates
	task   string
}

type reuseIdentity struct {
	scope, account, proxy, endpoint string
	token                           [32]byte
}

type wsFrame struct {
	kind websocket.MessageType
	data []byte
}

type reusableSocket struct {
	conn   *websocket.Conn
	stop   context.CancelFunc
	ctx    context.Context
	turn   atomic.Pointer[chan wsFrame]
	lastID string // owned by the current lease, published under the pool lock
}

func (s *reusableSocket) read(ctx context.Context, turn chan wsFrame) (websocket.MessageType, []byte, error) {
	select {
	case frame := <-turn:
		return frame.kind, frame.data, nil
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-s.ctx.Done():
		return 0, nil, net.ErrClosed
	}
}

func (s *reusableSocket) readLoop() {
	defer s.stop()
	for {
		kind, data, err := s.conn.Read(s.ctx)
		if err != nil {
			return
		}
		turn := s.turn.Load()
		var frame struct{ Type string }
		if kind == websocket.MessageText {
			_ = json.Unmarshal(data, &frame)
		}
		if turn == nil {
			if frame.Type == "basispoints.response.heartbeat" {
				continue
			}
			return
		}
		switch frame.Type {
		case "response.completed", "response.failed", "response.incomplete", "error":
			s.turn.Store(nil)
		}
		select {
		case *turn <- wsFrame{kind, data}:
		case <-s.ctx.Done():
			return
		}
	}
}

type idleSocket struct {
	socket   *reusableSocket
	identity reuseIdentity
	since    time.Time
	timer    *time.Timer
	unwatch  func() bool
}

const reuseIdleTTL = 30 * time.Second
const maxIdleReuseSockets = 64

// The task slot has already been acquired when a socket is taken or parked.
// Retain at most one idle socket per worker and cap the process-wide idle set.
var idleReuse = struct {
	sync.Mutex
	slots map[reuseSlot]*idleSocket
}{slots: make(map[reuseSlot]*idleSocket)}

func takeReusable(slot reuseSlot, identity reuseIdentity) *reusableSocket {
	idleReuse.Lock()
	defer idleReuse.Unlock()
	entry := idleReuse.slots[slot]
	entrySlot := slot
	if entry == nil && slot.task != "" {
		// A worker replacement can keep the same persistent task ID.
		for key, idle := range idleReuse.slots {
			if key.task == slot.task {
				entry, entrySlot = idle, key
				break
			}
		}
	}
	if entry == nil {
		return nil
	}
	delete(idleReuse.slots, entrySlot)
	entry.timer.Stop()
	entry.unwatch()
	if entrySlot == slot && identity.scope != "" && entry.identity == identity && entry.socket.ctx.Err() == nil &&
		time.Since(entry.since) < reuseIdleTTL {
		return entry.socket
	}
	entry.socket.stop()
	return nil
}

func parkReusable(slot reuseSlot, identity reuseIdentity, socket *reusableSocket) {
	idleReuse.Lock()
	defer idleReuse.Unlock()
	if socket.ctx.Err() != nil {
		return
	}
	var oldest reuseSlot
	for key, entry := range idleReuse.slots {
		if key.worker == slot.worker {
			oldest = key
			break
		}
		if oldest.worker == nil || entry.since.Before(idleReuse.slots[oldest].since) {
			oldest = key
		}
	}
	if oldest.worker != nil && (oldest.worker == slot.worker || len(idleReuse.slots) >= maxIdleReuseSockets) {
		entry := idleReuse.slots[oldest]
		delete(idleReuse.slots, oldest)
		entry.timer.Stop()
		entry.unwatch()
		entry.socket.stop()
	}
	entry := &idleSocket{socket: socket, identity: identity, since: time.Now()}
	idleReuse.slots[slot] = entry
	evict := func() {
		idleReuse.Lock()
		defer idleReuse.Unlock()
		if idleReuse.slots[slot] == entry {
			delete(idleReuse.slots, slot)
			entry.timer.Stop()
			entry.unwatch()
			socket.stop()
		}
	}
	entry.timer = time.AfterFunc(reuseIdleTTL, evict)
	entry.unwatch = context.AfterFunc(socket.ctx, evict)
}

func (s *Server) reuseLease(ctx context.Context, token, account string) (reuseSlot, reuseIdentity, *reuseScope) {
	task, _ := ctx.Value(upstreamTaskKey{}).(upstreamTaskIdentity)
	scope, _ := ctx.Value(reuseScopeKey{}).(*reuseScope)
	slot := reuseSlot{worker: s.startRates, task: task.taskID}
	if scope == nil || slot.task == "" {
		return slot, reuseIdentity{}, nil
	}
	return slot, reuseIdentity{scope.id, account, s.cfg.ProxyURL, s.cfg.Upstream, sha256.Sum256([]byte(token))}, scope
}

func (s *Server) openSocket(ctx context.Context, address string, headers http.Header, token, account string, scope *reuseScope, socket *reusableSocket) (*websocket.Conn, func(), func(), *reusableSocket, error) {
	if socket != nil {
		return socket.conn, socket.stop, socket.stop, socket, nil
	}
	if scope == nil {
		conn, release, abort, err := s.dialWebSocket(ctx, address, headers, token, account)
		return conn, release, abort, nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	stopDial := context.AfterFunc(ctx, cancel)
	// Keep the request's timing observation for the dial only. The idle socket
	// must not retain request context values or the checked credential snapshot.
	dialContext := context.WithValue(lifetime, webSocketTimingKey{}, webSocketTimingFromContext(ctx))
	conn, release, abort, err := s.dialWebSocket(dialContext, address, headers, token, account)
	stopDial()
	if err != nil {
		cancel()
		return nil, nil, nil, nil, err
	}
	var once sync.Once
	closeSocket := func() { once.Do(func() { cancel(); abort(); release() }) }
	socket = &reusableSocket{conn: conn, ctx: lifetime, stop: closeSocket}
	conn.SetReadLimit(maxWSMessage)
	return conn, closeSocket, closeSocket, socket, nil
}

func (b *webSocketBody) validateReuseEvent(kind string, event map[string]json.RawMessage) bool {
	var response struct{ ID string }
	_ = json.Unmarshal(event["response"], &response)
	switch kind {
	case "response.created":
		if b.reused && (b.responseID != "" || response.ID == "" || response.ID == b.socket.lastID) {
			return false
		}
		b.responseID = response.ID
	case "response.completed":
		if b.responseID != "" && response.ID == b.responseID {
			b.socket.lastID = response.ID
			b.reuseCompleted.Store(true)
		} else if b.reused {
			return false
		}
	default:
		if b.reused && kind != "error" {
			var id string
			_ = json.Unmarshal(event["response_id"], &id)
			if b.responseID == "" || (response.ID != "" && response.ID != b.responseID) ||
				(id != "" && id != b.responseID) {
				return false
			}
		}
	}
	return true
}
