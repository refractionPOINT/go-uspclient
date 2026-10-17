package uspclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/refractionPOINT/go-uspclient/protocol"
	"github.com/vmihailenco/msgpack/v5"
)

// This isolates the local client/transport from a source service. Each mode has
// 1000 concurrent source callbacks; admission-only callbacks return on Ship,
// while confirmed callbacks retain their slot until the receipt completes.
func BenchmarkReceiptTransport(b *testing.B) {
	for _, window := range []uint64{1000, 100000} {
		b.Run(fmt.Sprintf("window_%d", window), func(b *testing.B) {
			for _, confirmed := range []bool{false, true} {
				name := "admission"
				if confirmed {
					name = "confirmation"
				}
				b.Run(name, func(b *testing.B) {
					var received, ackWrites atomic.Uint64
					var lastDelivered atomic.Int64
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						conn, err := wsUpgrader.Upgrade(w, req, nil)
						if err != nil {
							return
						}
						defer conn.Close()
						if err := conn.ReadJSON(&protocol.ConnectionHeader{}); err != nil {
							return
						}
						if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageREADY}); err != nil {
							return
						}
						if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageFLOW, WindowSize: window}); err != nil {
							return
						}
						var writeMu sync.Mutex
						var tail atomic.Uint64
						ack := func(seq uint64) {
							if seq == 0 {
								return
							}
							writeMu.Lock()
							defer writeMu.Unlock()
							if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageACK, SeqNum: seq}); err == nil {
								ackWrites.Add(1)
							}
						}
						stop := make(chan struct{})
						var wg sync.WaitGroup
						wg.Add(1)
						go func() {
							defer wg.Done()
							timer := time.NewTicker(100 * time.Millisecond)
							defer timer.Stop()
							for {
								select {
								case <-stop:
									return
								case <-timer.C:
									ack(tail.Load())
								}
							}
						}()
						defer func() { close(stop); wg.Wait() }()
						for {
							_, raw, err := conn.ReadMessage()
							if err != nil {
								return
							}
							var msg protocol.DataMessage
							if err := msgpack.Unmarshal(raw, &msg); err != nil {
								return
							}
							tail.Store(msg.SeqNum)
							if received.Load()+1 == uint64(b.N) {
								lastDelivered.Store(time.Now().UnixNano())
							}
							received.Add(1)
							if msg.AckRequested {
								ack(msg.SeqNum)
							}
						}
					}))
					b.Cleanup(srv.Close)
					c, err := NewClient(context.Background(), ClientOptions{Identity: Identity{Oid: uuid.NewString(), InstallationKey: "test-key"}, DestURL: "ws" + strings.TrimPrefix(srv.URL, "http"), Platform: "text"})
					if err != nil {
						b.Fatal(err)
					}
					b.Cleanup(func() {
						if err := c.Drain(3 * time.Second); err != nil {
							b.Error(err)
						}
						if _, err := c.Close(); err != nil {
							b.Error(err)
						}
					})
					deadline := time.Now().Add(3 * time.Second)
					for c.ab.GetCurrentCapacity() != window {
						if time.Now().After(deadline) {
							b.Fatal("FLOW never arrived")
						}
						time.Sleep(time.Millisecond)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					b.SetParallelism(500)
					b.ResetTimer()
					start := time.Now()
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							m := &protocol.DataMessage{TextPayload: "payload"}
							if confirmed {
								r, err := c.ShipWithReceipt(m, 3*time.Second)
								if err == nil {
									err = r.Wait(ctx)
								}
								if err != nil {
									b.Error(err)
									cancel()
									return
								}
							} else if err := c.Ship(m, 3*time.Second); err != nil {
								b.Error(err)
								cancel()
								return
							}
						}
					})
					for received.Load() != uint64(b.N) {
						select {
						case <-ctx.Done():
							b.Fatalf("received=%d wanted=%d: %v", received.Load(), b.N, ctx.Err())
						case <-time.After(time.Millisecond):
						}
					}
					elapsed := time.Since(start)
					deliveredDuration := time.Unix(0, lastDelivered.Load()).Sub(start)
					b.StopTimer()
					b.ReportMetric(float64(b.N)/elapsed.Seconds(), "events/s")
					b.ReportMetric(float64(b.N)/deliveredDuration.Seconds(), "delivered-events/s")
					b.ReportMetric((elapsed-deliveredDuration).Seconds()*1000, "receipt-tail-ms")
					b.ReportMetric(float64(ackWrites.Load())/float64(b.N), "proxy-acks/event")
				})
			}
		})
	}
}
