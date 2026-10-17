package uspclient

import (
	"context"
	"errors"
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

func receiptPending(t *testing.T, receipt *Receipt) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := receipt.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unconfirmed receipt completed: %v", err)
	}
}

func receiptResult(t *testing.T, receipt *Receipt, want error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := receipt.Wait(ctx); !errors.Is(err, want) {
		t.Fatalf("receipt=%v want=%v", err, want)
	}
}

func TestReceiptCumulativeConfirmationAndCanceledWait(t *testing.T) {
	c, srv := drainClient(t, nil)
	first, err := c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "first"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ship(&protocol.DataMessage{TextPayload: "untracked"}, time.Second); err != nil {
		t.Fatal(err)
	}
	last, err := c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "last"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 3; seq++ {
		receiveSeq(t, srv, seq)
	}
	// A canceled wait must not withdraw accepted data or finish its receipt.
	receiptPending(t, first)
	if err := c.ab.Ack(4); err == nil {
		t.Fatal("out-of-range cumulative ACK accepted")
	}
	receiptPending(t, first)
	receiptPending(t, last)
	srv.ack(t, 2)
	receiptResult(t, first, nil)
	receiptPending(t, last)
	srv.ack(t, 3)
	receiptResult(t, last, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := first.Wait(ctx); err != nil {
		t.Fatalf("confirmed receipt lost to canceled wait: %v", err)
	}
	c.ab.RLock()
	remaining := len(c.ab.receipts)
	c.ab.RUnlock()
	if remaining != 0 {
		t.Fatalf("completed receipt metadata retained: %d", remaining)
	}
}

func TestReceiptReconnectAndCloseReturnsSourceOwnership(t *testing.T) {
	pool := NewByteBudget(testMsgSize)
	c, srv := drainClient(t, pool)
	receipt, err := c.ShipWithReceipt(textMsg(testMsgPayload), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	receiveSeq(t, srv, 1)
	if got, want := pool.Used(), testMsgSize+receiptOverheadBytes; got != want {
		t.Fatalf("receipt byte charge=%d want=%d", got, want)
	}
	receiptPending(t, receipt)
	srv.dropConnection()
	receiveSeq(t, srv, 1)
	if got, want := pool.Used(), testMsgSize+receiptOverheadBytes; got != want {
		t.Fatalf("reconnect receipt byte charge=%d want=%d", got, want)
	}
	receiptPending(t, receipt)
	// The full shared pool must reject another admission without retaining a
	// receipt. A never-ACKing peer must still hand ownership back on Close.
	if got, err := c.ShipWithReceipt(textMsg(testMsgPayload), 30*time.Millisecond); got != nil || !errors.Is(err, ErrorBufferFull) {
		t.Fatalf("full pool admission: receipt=%v err=%v", got, err)
	}
	if err := c.Drain(30 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	receiptPending(t, receipt)
	if messages, err := c.Close(); err != nil || len(messages) != 1 {
		t.Fatalf("Close=%d, %v", len(messages), err)
	}
	receiptResult(t, receipt, ErrorClosed)
	if pool.Used() != 0 {
		t.Fatalf("Close retained pool bytes: %d", pool.Used())
	}
}

func TestReceiptCloseJoinsSenderBeforeReturningSourceOwnership(t *testing.T) {
	c, srv := drainClient(t, nil)
	receipt, err := c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "owned-until-stopped"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	receiveSeq(t, srv, 1)
	// Model a sender still completing work after the socket was closed. Close
	// must join it before a source can Nack its unconfirmed message.
	var release sync.Once
	c.wg.Add(1)
	t.Cleanup(func() { release.Do(c.wg.Done) })
	done := make(chan error, 1)
	go func() { _, err := c.Close(); done <- err }()
	waitFor(t, "admission sealed", func() bool {
		c.ab.RLock()
		defer c.ab.RUnlock()
		return !c.ab.isRunning
	})
	receiptPending(t, receipt)
	release.Do(c.wg.Done)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not join the released sender")
	}
	receiptResult(t, receipt, ErrorClosed)
}

func TestReceiptAckCloseRaceAndAlreadyConfirmedOwnership(t *testing.T) {
	// Exercise the actual AckBuffer admission/ACK/Close locking, including
	// independent waiters on the same receipt and repeated Close.
	for attempt := 0; attempt < 100; attempt++ {
		b, err := NewAckBuffer(AckBufferOptions{})
		if err != nil {
			t.Fatal(err)
		}
		r := &Receipt{done: make(chan struct{})}
		if err := b.addWithReceipt(&protocol.DataMessage{}, time.Second, r); err != nil {
			t.Fatal(err)
		}
		b.GetNextToDeliver(time.Second)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); _ = b.Ack(1) }()
		go func() { defer wg.Done(); b.Close() }()
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := r.Wait(ctx); err != nil && !errors.Is(err, ErrorClosed) {
				t.Errorf("racing ownership: %v", err)
			}
		}()
		wg.Wait()
		b.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = r.Wait(ctx)
		cancel()
		if err != nil && !errors.Is(err, ErrorClosed) {
			t.Fatal(err)
		}
		if len(b.receipts) != 0 {
			t.Fatal("closed receipt metadata retained")
		}
	}
}

func TestReceiptImmediateAckDoesNotRaceRegistration(t *testing.T) {
	serverErrors := make(chan error, 1)
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
		if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageFLOW, WindowSize: 100000}); err != nil {
			return
		}
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var message protocol.DataMessage
			if err := msgpack.Unmarshal(raw, &message); err != nil {
				serverErrors <- err
				return
			}
			// No periodic ACK: a bounded source must request its own progress
			// even after the peer opens a very large window.
			if message.AckRequested {
				if err := conn.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageACK, SeqNum: message.SeqNum}); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(context.Background(), ClientOptions{
		Identity: Identity{Oid: uuid.NewString(), InstallationKey: "test-key"},
		DestURL:  "ws" + strings.TrimPrefix(srv.URL, "http"), Platform: "text",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	waitFor(t, "large FLOW", func() bool { return c.ab.GetCurrentCapacity() == 100000 })
	// A single callback must progress without waiting for 50,000 admissions.
	first, err := c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "single"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	receiptResult(t, first, nil)
	var wg sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				receipt, err := c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "concurrent"}, time.Second)
				if err != nil {
					t.Error(err)
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				err = receipt.Wait(ctx)
				cancel()
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-serverErrors:
		t.Fatal(err)
	default:
	}
}
