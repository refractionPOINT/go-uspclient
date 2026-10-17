package uspclient

import (
	"fmt"
	"testing"
	"time"

	"github.com/refractionPOINT/go-uspclient/protocol"
)

func TestAckBufferShrinkPreservesPendingReplayAndByteOwnership(t *testing.T) {
	pool := NewByteBudget(1 << 20)
	b, err := NewAckBuffer(AckBufferOptions{SharedBudget: pool})
	if err != nil {
		t.Fatal(err)
	}
	b.UpdateCapacity(100000)
	for i := 0; i < 200; i++ {
		if !b.Add(&protocol.DataMessage{TextPayload: fmt.Sprintf("pending-%d", i)}, time.Second) {
			t.Fatal("admission failed")
		}
		if b.GetNextToDeliver(0) == nil {
			t.Fatal("message not delivered")
		}
	}
	if err := b.Ack(64); err != nil {
		t.Fatal(err)
	}
	held := pool.Used()
	b.UpdateCapacity(1) // The real reconnect resets FLOW before replay.
	b.ResetDelivery()
	if len(b.buff) != 136 || len(b.sizes) != 136 || cap(b.sizes) != 136 {
		t.Fatalf("shrink discarded pending or retained old backing: messages=%d sizes=%d/%d", len(b.buff), len(b.sizes), cap(b.sizes))
	}
	if pool.Used() != held || b.BufferedBytes() != held {
		t.Fatal("reconnect changed ownership of unconfirmed bytes")
	}
	for seq := uint64(65); seq <= 200; seq++ {
		if message := b.GetNextToDeliver(0); message == nil || message.SeqNum != seq {
			t.Fatalf("replay sequence %d: %v", seq, message)
		}
	}
	b.UpdateCapacity(64)
	if err := b.Ack(136); err != nil {
		t.Fatal(err)
	}
	if len(b.buff) != 64 || cap(b.sizes) != 64 {
		t.Fatalf("ACK did not reclaim former window: messages=%d sizes=%d", len(b.buff), cap(b.sizes))
	}
	if b.isAvailable.IsSet() {
		t.Fatal("ACK reopened admission while the smaller window was full")
	}
	if b.Add(&protocol.DataMessage{}, time.Millisecond) {
		t.Fatal("ACK reopened admission while the smaller window was full")
	}
	if err := b.Ack(137); err != nil {
		t.Fatal(err)
	}
	if !b.Add(&protocol.DataMessage{TextPayload: "after-reconnect"}, time.Second) {
		t.Fatal("ACK did not free one admission slot")
	}
	if message := b.GetNextToDeliver(0); message == nil || message.SeqNum != 201 {
		t.Fatalf("new message sequence: %v", message)
	}
	if err := b.Ack(201); err != nil {
		t.Fatal(err)
	}
	if pool.Used() != 0 || b.BufferedBytes() != 0 {
		t.Fatal("confirmed messages retained byte ownership")
	}
	b.Close()
}

func TestAckBufferShrinksIdleAndZeroFlowWindows(t *testing.T) {
	b, err := NewAckBuffer(AckBufferOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b.UpdateCapacity(100000)
	b.UpdateCapacity(64)
	if len(b.buff) != 64 || cap(b.buff) != 64 {
		t.Fatalf("idle former window retained: %d/%d", len(b.buff), cap(b.buff))
	}
	if !b.Add(&protocol.DataMessage{TextPayload: "held"}, time.Second) || b.GetNextToDeliver(0) == nil {
		t.Fatal("message not delivered")
	}
	b.UpdateCapacity(0)
	if len(b.buff) != 1 || b.nextIndexFree != 1 {
		t.Fatal("zero FLOW discarded pending message")
	}
	if err := b.Ack(1); err != nil {
		t.Fatal(err)
	}
	if b.isAvailable.IsSet() {
		t.Fatal("ACK reopened admission at zero FLOW")
	}
	if b.Add(&protocol.DataMessage{}, time.Millisecond) {
		t.Fatal("ACK reopened admission at zero FLOW")
	}
	b.UpdateCapacity(1)
	if !b.Add(&protocol.DataMessage{}, time.Second) {
		t.Fatal("FLOW recovery did not wake admission")
	}
	b.Close()
}

func TestAckBufferShrinkReconnectDrainConfirmsSameTail(t *testing.T) {
	pool := NewByteBudget(1 << 20)
	c, server := drainClient(t, pool)
	server.mu.Lock()
	write := server.writeMsg
	server.mu.Unlock()
	if err := write(protocol.ControlMessage{Verb: protocol.ControlMessageFLOW, WindowSize: 100000}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "large FLOW", func() bool { return c.ab.GetCurrentCapacity() == 100000 })
	for i := 0; i < 3; i++ {
		if err := c.Ship(&protocol.DataMessage{TextPayload: "same-unconfirmed-tail"}, time.Second); err != nil {
			t.Fatal(err)
		}
		receiveSeq(t, server, uint64(i+1))
	}
	held := pool.Used()
	done := make(chan error, 1)
	go func() { done <- c.Drain(3 * time.Second) }()
	drainStillWaiting(t, done)
	server.dropConnection()
	for seq := uint64(1); seq <= 3; seq++ {
		receiveSeq(t, server, seq)
	}
	waitFor(t, "reconnected FLOW", func() bool { return c.ab.GetCurrentCapacity() == 1000 })
	c.ab.RLock()
	retained := len(c.ab.buff)
	c.ab.RUnlock()
	if retained != 1000 || pool.Used() != held {
		t.Fatalf("reconnect retained former FLOW or lost byte ownership: slots=%d bytes=%d want=%d", retained, pool.Used(), held)
	}
	drainStillWaiting(t, done)
	server.ack(t, 3)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnected drain missed the final ACK")
	}
	if messages, err := c.Close(); err != nil || len(messages) != 0 || pool.Used() != 0 {
		t.Fatalf("Close=%d/%v bytes=%d", len(messages), err, pool.Used())
	}
}

// One pending message reproduces frequent cumulative ACKs after FLOW has grown.
// Setup is excluded; the last cell also covers a formerly large reconnect.
func BenchmarkAckCycleLiveWindow(b *testing.B) {
	for _, tc := range []struct {
		name     string
		capacity uint64
		shrink   bool
	}{
		{"64", 64, false},
		{"16384", 16384, false},
		{"100000", 100000, false},
		{"100000_then_64", 100000, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			buffer, err := NewAckBuffer(AckBufferOptions{})
			if err != nil {
				b.Fatal(err)
			}
			buffer.UpdateCapacity(tc.capacity)
			if tc.shrink {
				buffer.UpdateCapacity(64)
			}
			message := &protocol.DataMessage{TextPayload: "one-pending"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !buffer.Add(message, 0) || buffer.GetNextToDeliver(0) == nil {
					b.Fatal("admit/deliver failed")
				}
				if err := buffer.Ack(message.SeqNum); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(len(buffer.buff)), "backing-slots")
		})
	}
}
