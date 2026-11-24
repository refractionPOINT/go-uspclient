package uspclient

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/refractionPOINT/go-uspclient/protocol"
	"github.com/vmihailenco/msgpack/v5"
)

func TestReconnectWithTransportDeadlines(t *testing.T) {
	oldDelay := reconnectRetryDelay.Swap(int64(20 * time.Millisecond))
	t.Cleanup(func() { reconnectRetryDelay.Store(oldDelay) })

	for _, name := range []string{"silent_socket", "missing_ready"} {
		t.Run(name, func(t *testing.T) {
			type delivery struct {
				attempt    int32
				instanceID string
				message    protocol.DataMessage
			}
			var attempts atomic.Int32
			deliveries := make(chan delivery, 2)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := wsUpgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				var header protocol.ConnectionHeader
				if err := conn.ReadJSON(&header); err != nil {
					return
				}
				attempt := attempts.Add(1)
				if name == "missing_ready" && attempt == 2 {
					<-release
					return
				}
				if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageREADY}); err != nil {
					return
				}
				for {
					_, raw, err := conn.ReadMessage()
					if err != nil {
						return
					}
					var message protocol.DataMessage
					if err := msgpack.Unmarshal(raw, &message); err != nil {
						return
					}
					select {
					case deliveries <- delivery{attempt, header.InstanceID, message}:
					case <-release:
						return
					}
					if attempt == 1 {
						if name == "silent_socket" {
							// Leave the socket open without ACKs, pongs or a close frame.
							<-release
						}
						return
					}
					if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageACK, SeqNum: message.SeqNum}); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			defer close(release)

			c, err := NewClient(t.Context(), ClientOptions{
				Identity:         Identity{Oid: uuid.NewString(), InstallationKey: "local"},
				DestURL:          "ws" + strings.TrimPrefix(server.URL, "http"),
				Platform:         "text",
				LivenessTimeout:  350 * time.Millisecond,
				ReconnectTimeout: 350 * time.Millisecond,
				BufferOptions:    AckBufferOptions{MaxBytes: 10000},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()

			if err := c.Ship(&protocol.DataMessage{TextPayload: "retained", AckRequested: true}, time.Second); err != nil {
				t.Fatal(err)
			}
			next := func() delivery {
				t.Helper()
				select {
				case got := <-deliveries:
					return got
				case <-time.After(3 * time.Second):
					t.Fatal("no delivery within the recovery budget")
					return delivery{}
				}
			}
			first, replay := next(), next()
			wantAttempt := int32(2)
			if name == "missing_ready" {
				wantAttempt = 3
			}
			if first.attempt != 1 || replay.attempt < wantAttempt || first.instanceID == "" ||
				replay.instanceID != first.instanceID || replay.message.SeqNum != first.message.SeqNum ||
				replay.message.TextPayload != first.message.TextPayload {
				t.Fatalf("first delivery %+v, replacement delivery %+v", first, replay)
			}
			if err := c.Drain(3 * time.Second); err != nil {
				t.Fatalf("replacement did not acknowledge replay: %v", err)
			}
			if used := c.BufferedBytes(); used != 0 {
				t.Fatalf("bytes retained after ACK: %d", used)
			}
		})
	}
}

func TestPeerPingsKeepBackpressuredSocketAlive(t *testing.T) {
	const liveness = 850 * time.Millisecond
	var connections atomic.Int32
	startPressure := make(chan struct{})
	heartbeatDone := make(chan struct{})
	replacement := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var header protocol.ConnectionHeader
		if err := conn.ReadJSON(&header); err != nil {
			return
		}
		num := connections.Add(1)
		if num == 1 {
			if err := conn.UnderlyingConn().(*net.TCPConn).SetReadBuffer(4 << 10); err != nil {
				return
			}
		}
		if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageREADY}); err != nil {
			return
		}
		if num != 1 {
			select {
			case replacement <- struct{}{}:
			default:
			}
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}
		select {
		case <-startPressure:
		case <-release:
			return
		}
		tick := time.NewTicker(75 * time.Millisecond)
		defer tick.Stop()
		for range 28 {
			select {
			case <-tick.C:
			case <-release:
				return
			}
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
				return
			}
		}
		close(heartbeatDone)
		<-release
	}))
	defer server.Close()
	defer close(release)

	client, err := NewClient(t.Context(), ClientOptions{
		Identity:        Identity{Oid: uuid.NewString(), InstallationKey: "local"},
		DestURL:         "ws" + strings.TrimPrefix(server.URL, "http"),
		Platform:        "text",
		LivenessTimeout: liveness,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.connMutex.Lock()
	tcp := client.conn.UnderlyingConn().(*net.TCPConn)
	client.connMutex.Unlock()
	if err := tcp.SetWriteBuffer(4 << 10); err != nil {
		t.Fatal(err)
	}
	// The peer never reads this body. Fill the TCP window so responding
	// to its pings can block on either the socket or the WebSocket writer.
	if err := client.Ship(&protocol.DataMessage{BinaryPayload: strings.Repeat("x", 8<<20)}, time.Second); err != nil {
		t.Fatal(err)
	}
	close(startPressure)
	select {
	case <-heartbeatDone:
	case <-replacement:
		t.Fatal("replaced a live peer that could send but was too busy to read")
	case <-time.After(6 * time.Second):
		t.Fatal("peer heartbeat did not finish")
	}
	if connections.Load() != 1 {
		t.Fatal("client reconnected while peer heartbeats were arriving")
	}
	select {
	case <-replacement:
	case <-time.After(3 * time.Second):
		t.Fatal("client did not replace the socket after its last heartbeat")
	}
}
func TestOptInRecoveryCarriesBackoff(t *testing.T) {
	var connections atomic.Int32
	backingOff := make(chan time.Time, 1)
	replacement := make(chan time.Time, 1)
	sent := make(chan time.Time, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var header protocol.ConnectionHeader
		if conn.ReadJSON(&header) != nil {
			return
		}
		attempt := connections.Add(1)
		if conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageREADY}) != nil {
			return
		}
		if attempt == 1 {
			if conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageBACKOFF, Duration: 2}) != nil {
				return
			}
		} else {
			replacement <- time.Now()
		}
		if _, _, err := conn.ReadMessage(); err == nil && attempt == 2 {
			sent <- time.Now()
		}
	}))
	defer server.Close()
	c, err := NewClient(t.Context(), ClientOptions{
		Identity:         Identity{Oid: uuid.NewString(), InstallationKey: "fixture"},
		Platform:         "text",
		DestURL:          "ws" + strings.TrimPrefix(server.URL, "http"),
		LivenessTimeout:  time.Second,
		ReconnectTimeout: time.Second,
		DebugLog: func(m string) {
			if m == "backing off 2 seconds" {
				backingOff <- time.Now()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	wait := func(ch <-chan time.Time, d time.Duration, why string) time.Time {
		t.Helper()
		select {
		case at := <-ch:
			return at
		case <-time.After(d):
			t.Fatalf("timed out waiting for %s", why)
			return time.Time{}
		}
	}
	start := wait(backingOff, 2*time.Second, "receiver backoff")
	c.Reconnect()
	wait(replacement, time.Until(start.Add(2*time.Second)), "socket replacement before backoff ends")
	if err := c.Ship(&protocol.DataMessage{TextPayload: "retained during receiver backoff"}, time.Second); err != nil {
		t.Fatal(err)
	}
	at := wait(sent, 3*time.Second, "send after original backoff")
	if at.Sub(start) < 2*time.Second {
		t.Fatalf("replacement dropped the receiver backoff; sent at %v", at.Sub(start))
	}
}
