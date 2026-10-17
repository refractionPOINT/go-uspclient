package uspclient

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/refractionPOINT/go-uspclient/protocol"
	"github.com/vmihailenco/msgpack/v5"
)

// A second transport failure can happen as soon as the new workers start.
// Hold the connected debug callback to make that real scheduling window
// deterministic; neither the reconnect CAS nor the keepalive timer is mocked.
func TestReconnectWorkerStartupFailure(t *testing.T) {
	for _, draining := range []bool{false, true} {
		name := "sending"
		if draining {
			name = "draining"
		}
		t.Run(name, func(t *testing.T) { testReconnectWorkerStartupFailure(t, draining) })
	}
}

func testReconnectWorkerStartupFailure(t *testing.T, draining bool) {
	t.Helper()
	const payload = "retained-across-startup-failure"
	type peer struct {
		n    int32
		conn *websocket.Conn
	}
	type delivery struct {
		n       int32
		message protocol.DataMessage
	}
	var attempts, connected, listeners, senders, disconnects atomic.Int32
	var connections sync.Map
	peers := make(chan peer, 8)
	deliveries := make(chan delivery, 8)
	errors := make(chan error, 8)
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	releaseThird := make(chan struct{})
	var secondOnce, thirdOnce sync.Once
	unblockSecond := func() { secondOnce.Do(func() { close(releaseSecond) }) }
	unblockThird := func() { thirdOnce.Do(func() { close(releaseThird) }) }
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connections.Store(conn, struct{}{})
		defer connections.Delete(conn)
		defer conn.Close()
		n := attempts.Add(1)
		var header protocol.ConnectionHeader
		if err := conn.ReadJSON(&header); err != nil {
			return
		}
		if header.Hostname != "worker-startup" || header.DataFormat != "msgpack" || header.IsCompressed {
			errors <- fmt.Errorf("unexpected connection header: %+v", header)
			return
		}
		peers <- peer{n, conn}
		if n == 3 {
			<-releaseThird
		}
		if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageREADY}); err != nil {
			return
		}
		if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageFLOW, WindowSize: 1}); err != nil {
			return
		}
		for {
			kind, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.BinaryMessage {
				errors <- fmt.Errorf("unexpected frame kind %d", kind)
				return
			}
			var message protocol.DataMessage
			if err := msgpack.NewDecoder(bytes.NewReader(raw)).Decode(&message); err != nil {
				errors <- err
				return
			}
			// The ACK is issued only after receiving and decoding this frame.
			if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageACK, SeqNum: message.SeqNum}); err != nil {
				return
			}
			deliveries <- delivery{n, message}
		}
	}))
	var c *Client
	defer func() {
		unblockSecond()
		unblockThird()
		if c != nil {
			_, _ = c.Close()
		}
		connections.Range(func(key, _ any) bool { _ = key.(*websocket.Conn).Close(); return true })
		srv.Close()
	}()
	var err error
	c, err = NewClient(context.Background(), ClientOptions{
		Identity:     Identity{Oid: uuid.NewString(), InstallationKey: uuid.NewString()},
		Hostname:     "worker-startup",
		Platform:     "text",
		Architecture: "usp_adapter",
		DestURL:      "ws" + strings.TrimPrefix(srv.URL, "http") + "/usp",
		DebugLog: func(message string) {
			switch message {
			case "usp-client connected":
				if connected.Add(1) == 2 {
					close(secondStarted)
					<-releaseSecond
				}
			case "listener exited":
				listeners.Add(1)
			case "sender exited":
				senders.Add(1)
			case "usp-client disconnecting":
				disconnects.Add(1)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wait := func(label string, predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !predicate() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: attempts=%d disconnects=%d listeners=%d senders=%d reconnect=%d", label, attempts.Load(), disconnects.Load(), listeners.Load(), senders.Load(), atomic.LoadUint32(&c.isReconnecting))
			}
			time.Sleep(time.Millisecond)
		}
	}
	nextPeer := func(want int32) peer {
		t.Helper()
		select {
		case p := <-peers:
			if p.n != want {
				t.Fatalf("connection %d, expected %d", p.n, want)
			}
			return p
		case err := <-errors:
			t.Fatal(err)
		case <-time.After(3 * time.Second):
			t.Fatalf("connection %d did not arrive before the keepalive fallback", want)
		}
		return peer{}
	}
	reset := func(p peer) {
		t.Helper()
		tcp, ok := p.conn.UnderlyingConn().(*net.TCPConn)
		if !ok {
			t.Fatalf("expected real TCP, got %T", p.conn.UnderlyingConn())
		}
		if err := tcp.SetLinger(0); err != nil {
			t.Fatal(err)
		}
		_ = tcp.Close()
	}
	reset(nextPeer(1))
	second := nextPeer(2)
	select {
	case <-secondStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("second workers did not start")
	}
	reset(second)
	wait("second reader exit", func() bool { return listeners.Load() >= 2 })
	if err := c.Ship(&protocol.DataMessage{TextPayload: payload}, time.Second); err != nil {
		t.Fatal(err)
	}
	wait("second sender exit", func() bool { return senders.Load() >= 2 })
	wait("failure handed to the next reconnect owner while startup is held", func() bool { return disconnects.Load() >= 2 })
	pending, err := c.GetUnsent()
	if err != nil || len(pending) != 1 || pending[0].TextPayload != payload {
		t.Fatalf("accepted event not retained: %v, %v", pending, err)
	}
	drainDone := make(chan error, 1)
	if draining {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		go func() { drainDone <- c.DrainContext(ctx) }()
	}
	unblockSecond()
	_ = nextPeer(3) // Hold READY so the next owner must remain exclusive.
	if atomic.LoadUint32(&c.isReconnecting) != 1 {
		t.Fatal("returning startup owner cleared the newer reconnect owner's flag")
	}
	var requests sync.WaitGroup
	for range 32 {
		requests.Add(1)
		go func() { defer requests.Done(); c.Reconnect() }()
	}
	requests.Wait()
	if disconnects.Load() != 2 || attempts.Load() != 3 {
		t.Fatalf("reconnect requests did not coalesce: disconnects=%d attempts=%d", disconnects.Load(), attempts.Load())
	}
	unblockThird()
	select {
	case got := <-deliveries:
		if got.n != 3 || got.message.TextPayload != payload || got.message.SeqNum != pending[0].SeqNum {
			t.Fatalf("same accepted event was not replayed: %+v", got)
		}
	case err := <-errors:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("retained event was not received and ACKed")
	}
	wait("real ACK clears the retained event", func() bool { p, err := c.GetUnsent(); return err == nil && len(p) == 0 })
	if draining {
		select {
		case err := <-drainDone:
			if err != nil {
				t.Fatalf("drain did not recover through the startup failure: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("drain did not finish after the real ACK")
		}
	}
	wait("reconnect finishes", func() bool { return atomic.LoadUint32(&c.isReconnecting) == 0 })
}
