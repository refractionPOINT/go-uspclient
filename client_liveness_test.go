package uspclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
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
			if first.attempt != 1 || replay.attempt != wantAttempt || first.instanceID == "" ||
				replay.instanceID != first.instanceID || replay.message.SeqNum != first.message.SeqNum ||
				replay.message.TextPayload != first.message.TextPayload {
				t.Fatalf("first delivery %+v, replacement delivery %+v", first, replay)
			}
			if err := c.Drain(time.Second); err != nil {
				t.Fatalf("replacement did not acknowledge replay: %v", err)
			}
			if used := c.BufferedBytes(); used != 0 {
				t.Fatalf("bytes retained after ACK: %d", used)
			}
		})
	}
}
