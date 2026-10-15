package uspclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/refractionPOINT/go-uspclient/protocol"
	"github.com/vmihailenco/msgpack/v5"
)

// A fake backend whose acks are driven by the test.
type ackDrivenServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	conn     interface{ Close() error }
	writeMsg func(v interface{}) error
	conns    int

	received chan uint64 // seq of every data message received, per delivery
}

func newAckDrivenServer(t *testing.T) *ackDrivenServer {
	s := &ackDrivenServer{received: make(chan uint64, 1000)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		hdr := protocol.ConnectionHeader{}
		if err := conn.ReadJSON(&hdr); err != nil {
			return
		}
		var wmu sync.Mutex
		write := func(v interface{}) error {
			wmu.Lock()
			defer wmu.Unlock()
			return conn.WriteJSON(v)
		}
		if err := write(protocol.ControlMessage{Verb: protocol.ControlMessageREADY}); err != nil {
			return
		}
		if err := write(protocol.ControlMessage{Verb: protocol.ControlMessageFLOW, WindowSize: 1000}); err != nil {
			return
		}
		s.mu.Lock()
		s.conn = conn
		s.writeMsg = write
		s.conns++
		s.mu.Unlock()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			m := protocol.DataMessage{}
			if err := msgpack.Unmarshal(raw, &m); err != nil {
				return
			}
			s.received <- m.SeqNum
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *ackDrivenServer) url() string { return "ws" + strings.TrimPrefix(s.srv.URL, "http") + "/usp" }

func (s *ackDrivenServer) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

func (s *ackDrivenServer) ack(t *testing.T, seq uint64) {
	t.Helper()
	s.mu.Lock()
	w := s.writeMsg
	s.mu.Unlock()
	if err := w(protocol.ControlMessage{Verb: protocol.ControlMessageACK, SeqNum: seq}); err != nil {
		t.Fatalf("ack: %v", err)
	}
}

func (s *ackDrivenServer) dropConnection() {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	c.Close()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// End to end through a real client: the byte cap is only released by backend
// acks, survives a reconnect (unacked messages are resent without being
// counted twice) and the share goes back to the pool on Close.
func TestClientByteBudgetEndToEnd(t *testing.T) {
	oldDelay := reconnectRetryDelay.Swap(int64(20 * time.Millisecond))
	t.Cleanup(func() { reconnectRetryDelay.Store(oldDelay) })

	srv := newAckDrivenServer(t)
	pool := NewByteBudget(100 * testMsgSize)
	c, err := NewClient(context.Background(), ClientOptions{
		Identity:      Identity{Oid: uuid.NewString(), InstallationKey: "iid"},
		DestURL:       srv.url(),
		Hostname:      "host",
		Platform:      "text",
		Architecture:  "usp_adapter",
		BufferOptions: AckBufferOptions{MaxBytes: 3 * testMsgSize, SharedBudget: pool},
	})
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	t.Cleanup(func() { c.Close() })
	waitFor(t, "FLOW window", func() bool { return c.ab.GetCurrentCapacity() == 1000 })

	for i := 0; i < 3; i++ {
		if err := c.Ship(textMsg(testMsgPayload), time.Second); err != nil {
			t.Fatalf("Ship(%d): %v", i, err)
		}
	}
	if err := c.Ship(textMsg(testMsgPayload), 150*time.Millisecond); err != ErrorBufferFull {
		t.Fatalf("4th Ship = %v, want ErrorBufferFull: the backend has acked nothing", err)
	}
	if c.BufferedBytes() != 3*testMsgSize || pool.Used() != 3*testMsgSize {
		t.Fatalf("buffered=%d pool=%d, want %d", c.BufferedBytes(), pool.Used(), 3*testMsgSize)
	}

	expectDeliveries := func(n int, what string) {
		t.Helper()
		for i := 0; i < n; i++ {
			select {
			case <-srv.received:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: only %d of %d deliveries seen", what, i, n)
			}
		}
	}
	expectDeliveries(3, "first delivery")

	// Lose the connection: the three unacked messages are resent, not recounted.
	srv.dropConnection()
	waitFor(t, "reconnect", func() bool { return srv.connCount() == 2 })
	expectDeliveries(3, "resend after reconnect")
	if c.BufferedBytes() != 3*testMsgSize || pool.Used() != 3*testMsgSize {
		t.Fatalf("after reconnect buffered=%d pool=%d, want %d", c.BufferedBytes(), pool.Used(), 3*testMsgSize)
	}

	shipped := make(chan error, 1)
	go func() { shipped <- c.Ship(textMsg(testMsgPayload), time.Minute) }()
	select {
	case err := <-shipped:
		t.Fatalf("Ship returned %v before the backend acked anything", err)
	case <-time.After(150 * time.Millisecond):
	}
	srv.ack(t, 2) // frees two messages
	select {
	case err := <-shipped:
		if err != nil {
			t.Fatalf("Ship after ack: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Ship never woke up after the backend acked")
	}
	if c.BufferedBytes() != 2*testMsgSize || pool.Used() != 2*testMsgSize {
		t.Fatalf("after ack buffered=%d pool=%d, want %d", c.BufferedBytes(), pool.Used(), 2*testMsgSize)
	}

	if _, err := c.Close(); err != nil {
		t.Logf("Close(): %v", err)
	}
	if pool.Used() != 0 {
		t.Fatalf("closed client still holds %d bytes of the pool", pool.Used())
	}
}
