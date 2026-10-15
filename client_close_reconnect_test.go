package uspclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/refractionPOINT/go-uspclient/protocol"
)

// A client that is reconnecting while the server refuses connections must stop
// trying once Close() is called. Before the fix the reconnect loop never
// looked at Close(), so it kept dialing and, as soon as the server accepted
// again, resurrected a live connection that nobody owned or could close.
func TestCloseStopsReconnectLoop(t *testing.T) {
	oldDelay := reconnectRetryDelay.Swap(int64(20 * time.Millisecond))
	t.Cleanup(func() { reconnectRetryDelay.Store(oldDelay) })

	var accepted, rejected atomic.Int32
	var refuse atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() {
			rejected.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		hdr := protocol.ConnectionHeader{}
		if err := conn.ReadJSON(&hdr); err != nil {
			return
		}
		if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageREADY}); err != nil {
			return
		}
		if accepted.Add(1) == 1 {
			// First connection: drop it and refuse the retries, like a proxy
			// that is shedding load.
			refuse.Store(true)
			return
		}
		// Any later connection stays up until the test ends.
		for {
			conn.SetReadDeadline(time.Now().Add(time.Minute))
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(context.Background(), ClientOptions{
		Identity:     Identity{Oid: uuid.NewString(), InstallationKey: "iid"},
		DestURL:      "ws" + strings.TrimPrefix(srv.URL, "http") + "/usp",
		Hostname:     "host",
		Platform:     "text",
		Architecture: "usp_adapter",
	})
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}

	// Wait until the client is demonstrably stuck in its reconnect loop.
	deadline := time.Now().Add(10 * time.Second)
	for rejected.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("client never started retrying (rejected=%d)", rejected.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := c.Close(); err != nil {
		t.Logf("Close(): %v", err)
	}

	// The server recovers. A closed client must not come back.
	refuse.Store(false)
	time.Sleep(20 * time.Duration(reconnectRetryDelay.Load()))
	if n := accepted.Load(); n != 1 {
		t.Fatalf("closed client reconnected: %d connections accepted, want 1", n)
	}
	if atomic.LoadUint32(&c.isReconnecting) != 0 {
		t.Fatal("reconnect loop still running after Close()")
	}
}

// Close() must also cover a reconnect that is mid-dial: a connection that
// completes after Close() has to be torn down, not kept.
func TestCloseDuringReconnectDoesNotLeakConnection(t *testing.T) {
	oldDelay := reconnectRetryDelay.Swap(int64(20 * time.Millisecond))
	t.Cleanup(func() { reconnectRetryDelay.Store(oldDelay) })

	var accepted atomic.Int32
	var live atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		hdr := protocol.ConnectionHeader{}
		if err := conn.ReadJSON(&hdr); err != nil {
			return
		}
		// Slow READY so a reconnect is still connecting when Close() runs.
		if accepted.Add(1) > 1 {
			time.Sleep(200 * time.Millisecond)
		}
		if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageREADY}); err != nil {
			return
		}
		live.Add(1)
		defer live.Add(-1)
		if accepted.Load() == 1 {
			// Trigger the client's reconnect.
			conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageRECONNECT})
		}
		for {
			conn.SetReadDeadline(time.Now().Add(time.Minute))
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(context.Background(), ClientOptions{
		Identity:     Identity{Oid: uuid.NewString(), InstallationKey: "iid"},
		DestURL:      "ws" + strings.TrimPrefix(srv.URL, "http") + "/usp",
		Hostname:     "host",
		Platform:     "text",
		Architecture: "usp_adapter",
	})
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}

	// Second connection attempt is in flight (header seen, READY delayed).
	deadline := time.Now().Add(10 * time.Second)
	for accepted.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("client never reconnected")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := c.Close(); err != nil {
		t.Logf("Close(): %v", err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for live.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d connection(s) still open after Close()", live.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if accepted.Load() > 2 {
		t.Fatalf("client kept dialing after Close(): %d connections", accepted.Load())
	}
}

// Hosts commonly treat any OnError as fatal and Close() the client from that
// callback. The callback fires from inside the reconnect loop itself (failed
// dial), so the loop must notice the Close() it triggered and stop.
func TestCloseFromOnErrorStopsReconnectLoop(t *testing.T) {
	oldDelay := reconnectRetryDelay.Swap(int64(20 * time.Millisecond))
	t.Cleanup(func() { reconnectRetryDelay.Store(oldDelay) })

	var accepted, rejected atomic.Int32
	var refuse atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() {
			rejected.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		hdr := protocol.ConnectionHeader{}
		if err := conn.ReadJSON(&hdr); err != nil {
			return
		}
		if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageREADY}); err != nil {
			return
		}
		if accepted.Add(1) == 1 {
			refuse.Store(true)
			return
		}
		for {
			conn.SetReadDeadline(time.Now().Add(time.Minute))
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	var client atomic.Pointer[Client]
	var closeOnce sync.Once
	closed := make(chan struct{})
	c, err := NewClient(context.Background(), ClientOptions{
		Identity:     Identity{Oid: uuid.NewString(), InstallationKey: "iid"},
		DestURL:      "ws" + strings.TrimPrefix(srv.URL, "http") + "/usp",
		Hostname:     "host",
		Platform:     "text",
		Architecture: "usp_adapter",
		OnError: func(error) {
			closeOnce.Do(func() {
				go func() {
					if cl := client.Load(); cl != nil {
						cl.Close()
					}
					close(closed)
				}()
			})
		},
	})
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	client.Store(c)

	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("OnError never fired from the reconnect loop")
	}

	refuse.Store(false)
	time.Sleep(20 * time.Duration(reconnectRetryDelay.Load()))
	if n := accepted.Load(); n != 1 {
		t.Fatalf("client closed from OnError reconnected anyway: %d connections, want 1", n)
	}
}

// Close() waits for an in-flight connect(); it must not sit out the READY
// timeout when the server is slow to answer.
func TestCloseDoesNotWaitForSlowReady(t *testing.T) {
	oldDelay := reconnectRetryDelay.Swap(int64(20 * time.Millisecond))
	t.Cleanup(func() { reconnectRetryDelay.Store(oldDelay) })

	var accepted atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		hdr := protocol.ConnectionHeader{}
		if err := conn.ReadJSON(&hdr); err != nil {
			return
		}
		n := accepted.Add(1)
		if n > 1 {
			// Never answer READY on a reconnect.
			<-release
			return
		}
		conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageREADY})
		conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageRECONNECT})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	c, err := NewClient(context.Background(), ClientOptions{
		Identity:     Identity{Oid: uuid.NewString(), InstallationKey: "iid"},
		DestURL:      "ws" + strings.TrimPrefix(srv.URL, "http") + "/usp",
		Hostname:     "host",
		Platform:     "text",
		Architecture: "usp_adapter",
	})
	if err != nil {
		t.Fatalf("NewClient(): %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for accepted.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("client never reconnected")
		}
		time.Sleep(5 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() blocked on a reconnect waiting for READY")
	}
}
