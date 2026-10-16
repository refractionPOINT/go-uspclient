package uspclient

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/refractionPOINT/go-uspclient/protocol"
)

func drainClient(t *testing.T, pool *ByteBudget) (*Client, *ackDrivenServer) {
	t.Helper()
	srv := newAckDrivenServer(t)
	c, err := NewClient(context.Background(), ClientOptions{
		Identity: Identity{Oid: uuid.NewString(), InstallationKey: "iid"}, DestURL: srv.url(), Platform: "text",
		BufferOptions: AckBufferOptions{SharedBudget: pool},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	waitFor(t, "FLOW", func() bool { return c.ab.GetCurrentCapacity() == 1000 })
	return c, srv
}

func receiveSeq(t *testing.T, srv *ackDrivenServer, want uint64) {
	t.Helper()
	select {
	case seq := <-srv.received:
		if seq != want {
			t.Fatalf("seq=%d want=%d", seq, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("data not received")
	}
}

func drainStillWaiting(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("Drain returned before final ACK: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
}

// The server has read everything but intentionally withholds the cumulative
// confirmation, as a proxy waiting for its downstream would do.
func TestDrainWaitsForFinalAck(t *testing.T) {
	c, srv := drainClient(t, nil)
	for i := 0; i < 3; i++ {
		if err := c.Ship(&protocol.DataMessage{TextPayload: "event"}, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	for i := uint64(1); i <= 3; i++ {
		receiveSeq(t, srv, i)
	}
	done := make(chan error, 1)
	go func() { done <- c.Drain(3 * time.Second) }()
	drainStillWaiting(t, done)
	srv.ack(t, 2)
	drainStillWaiting(t, done)
	srv.ack(t, 3)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Drain missed final ACK")
	}
	if unacked, err := c.Close(); err != nil || len(unacked) != 0 {
		t.Fatalf("Close=%d, %v", len(unacked), err)
	}
}

// A never-ACKing peer must produce an actionable error and leave payloads
// available to Close. Drain must not make the live byte budget appear empty.
func TestDrainNeverAckFullBudget(t *testing.T) {
	pool := NewByteBudget(testMsgSize)
	c, srv := drainClient(t, pool)
	if err := c.Ship(textMsg(testMsgPayload), time.Second); err != nil {
		t.Fatal(err)
	}
	receiveSeq(t, srv, 1)
	blocked := make(chan error, 1)
	go func() { blocked <- c.Ship(textMsg(testMsgPayload), 0) }()
	start := time.Now()
	err := c.Drain(120 * time.Millisecond)
	var de *DrainError
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &de) || de.Unacked != 1 || de.FirstSeq != 1 || de.LastSeq != 1 {
		t.Fatalf("Drain=%v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Drain exceeded bound: %s", elapsed)
	}
	select {
	case err := <-blocked:
		if !errors.Is(err, ErrorClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Ship not woken")
	}
	if pool.Used() != testMsgSize {
		t.Fatalf("retained messages lost budget charge: %d", pool.Used())
	}
	messages, err := c.Close()
	if err != nil || len(messages) != 1 || messages[0].TextPayload != textMsg(testMsgPayload).TextPayload {
		t.Fatalf("Close=%v, %v", messages, err)
	}
	if pool.Used() != 0 {
		t.Fatalf("Close leaked budget: %d", pool.Used())
	}
}

func TestDrainContextCancellation(t *testing.T) {
	c, srv := drainClient(t, nil)
	if err := c.Ship(&protocol.DataMessage{TextPayload: "event"}, time.Second); err != nil {
		t.Fatal(err)
	}
	receiveSeq(t, srv, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.DrainContext(ctx) }()
	drainStillWaiting(t, done)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Drain ignored cancellation")
	}
}

func TestDrainReconnectsBeforeConfirmation(t *testing.T) {
	c, srv := drainClient(t, nil)
	if err := c.Ship(&protocol.DataMessage{TextPayload: "event"}, time.Second); err != nil {
		t.Fatal(err)
	}
	receiveSeq(t, srv, 1)
	done := make(chan error, 1)
	go func() { done <- c.Drain(3 * time.Second) }()
	drainStillWaiting(t, done)
	srv.dropConnection()
	receiveSeq(t, srv, 1)
	drainStillWaiting(t, done)
	srv.ack(t, 1)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Drain did not finish after reconnect ACK")
	}
}

func TestDrainBackoffDoesNotHangClose(t *testing.T) {
	c, srv := drainClient(t, nil)
	srv.mu.Lock()
	write := srv.writeMsg
	srv.mu.Unlock()
	if err := write(protocol.ControlMessage{Verb: protocol.ControlMessageBACKOFF, Duration: 3}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "BACKOFF received", func() bool { return atomic.LoadUint64(&c.backoffTime) == 3 })
	if err := c.Ship(&protocol.DataMessage{TextPayload: "first"}, time.Second); err != nil {
		t.Fatal(err)
	}
	// The sender may already be waiting in GetNextToDeliver, so the first
	// message can pass before it observes BACKOFF. The second cannot pass.
	waitFor(t, "BACKOFF consumed", func() bool { return atomic.LoadUint64(&c.backoffTime) == 0 })
	if err := c.Ship(&protocol.DataMessage{TextPayload: "tail"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := c.Drain(100 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain=%v", err)
	}
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		// Unblock the old sender to let this regression test terminate cleanly.
		c.isStop.Set()
		t.Fatal("Close hung during server BACKOFF")
	}
}

func TestDrainCloseInterruptsWait(t *testing.T) {
	c, srv := drainClient(t, nil)
	if err := c.Ship(&protocol.DataMessage{TextPayload: "event"}, time.Second); err != nil {
		t.Fatal(err)
	}
	receiveSeq(t, srv, 1)
	done := make(chan error, 1)
	go func() { done <- c.Drain(3 * time.Second) }()
	drainStillWaiting(t, done)
	c.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrorClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not wake Drain")
	}
}

// A queued tail below the normal request interval must explicitly ask for
// confirmation. Already-delivered tails use the peer's periodic cumulative ACK.
func TestDrainRequestsAckForQueuedTail(t *testing.T) {
	b, _ := NewAckBuffer(AckBufferOptions{})
	b.UpdateCapacity(100)
	if !b.Add(&protocol.DataMessage{TextPayload: "tail"}, time.Second) {
		t.Fatal("Add")
	}
	done := make(chan error, 1)
	go func() { done <- (&Client{ab: b}).Drain(time.Second) }()
	waitFor(t, "sealed admission", func() bool { b.RLock(); defer b.RUnlock(); return !b.isRunning })
	msg := b.GetNextToDeliver(time.Second)
	if msg == nil || !msg.AckRequested {
		t.Fatalf("draining tail did not request ACK: %+v", msg)
	}
	drainStillWaiting(t, done)
	if err := b.Ack(msg.SeqNum); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBeginDrainWakesProducerWithoutLosingAcceptedData(t *testing.T) {
	pool := NewByteBudget(testMsgSize)
	c, srv := drainClient(t, pool)
	if err := c.Ship(textMsg(testMsgPayload), time.Second); err != nil {
		t.Fatal(err)
	}
	receiveSeq(t, srv, 1)
	blocked := make(chan error, 1)
	go func() { blocked <- c.Ship(textMsg(testMsgPayload), 0) }()
	c.BeginDrain()
	select {
	case err := <-blocked:
		if !errors.Is(err, ErrorClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("BeginDrain did not wake producer")
	}
	if pool.Used() != testMsgSize {
		t.Fatal("BeginDrain released retained bytes")
	}
	done := make(chan error, 1)
	go func() { done <- c.Drain(time.Second) }()
	drainStillWaiting(t, done)
	srv.ack(t, 1)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n, err := c.Close(); len(n) != 0 || err != nil {
		t.Fatalf("Close=%d,%v", len(n), err)
	}
}
