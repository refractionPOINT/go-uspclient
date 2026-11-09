package uspclient

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/refractionPOINT/go-uspclient/protocol"
)

func confirmationClient(t *testing.T) *Client {
	t.Helper()
	b, err := NewAckBuffer(AckBufferOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b.UpdateCapacity(8)
	c := &Client{ab: b, isStop: NewEvent()}
	t.Cleanup(func() { c.Close() })
	return c
}
func confirmationShip(t *testing.T, c *Client) AdmissionReceipt {
	t.Helper()
	r, err := c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "synthetic fragment"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func confirmationContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	return ctx
}
func confirmationPending(t *testing.T, c *Client, receipts ...AdmissionReceipt) <-chan error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() { result <- c.WaitForReceipts(ctx, receipts...) }()
	receiptWait(t, "barrier registered", func() bool { c.ab.RLock(); defer c.ab.RUnlock(); return c.ab.wake != nil })
	return result
}
func confirmationResult(t *testing.T, result <-chan error, want error) {
	t.Helper()
	select {
	case err := <-result:
		if !errors.Is(err, want) {
			t.Fatalf("barrier = %v, want %v", err, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("barrier did not wake")
	}
}
func TestConfirmationAllFragmentsAndNonterminal(t *testing.T) {
	c := confirmationClient(t)
	a, b := confirmationShip(t, c), confirmationShip(t, c)
	later := confirmationShip(t, c)
	for range 3 {
		c.ab.GetNextToDeliver(0)
	}
	result := confirmationPending(t, c, a, b)
	if err := c.ab.Ack(1); err != nil {
		t.Fatal(err)
	}
	// Cancellation is a deterministic check that the incomplete fragment set
	// cannot confirm, regardless of whether the background waiter has run yet.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := c.WaitForReceipts(ctx, a, b); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("partial fragments = %v", err)
	}
	if err := c.ab.Ack(2); err != nil {
		t.Fatal(err)
	}
	confirmationResult(t, result, nil)
	if c.ObserveReceipt(later).Frame != ReceiptRetained {
		t.Fatal("unrelated frame confirmed")
	}
	confirmationShip(t, c) // A successful barrier must leave admission open.
	if err := c.WaitForReceipts(confirmationContext(t), a, b); err != nil {
		t.Fatal(err)
	}
}
func TestConfirmationInvalidAckAndTokens(t *testing.T) {
	c := confirmationClient(t)
	r := confirmationShip(t, c)
	c.ab.GetNextToDeliver(0)
	// OnAck happens before validation. Even observing from that callback must
	// not treat the callback as confirmation.
	calls := 0
	c.ab.onAck = func() {
		calls++
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.WaitForReceipts(ctx, r); !errors.Is(err, context.Canceled) {
			t.Errorf("pre-validation wait = %v", err)
		}
	}
	result := confirmationPending(t, c, r)
	if c.ab.Ack(99) == nil {
		t.Fatal("invalid ACK accepted")
	}
	if c.ObserveReceipt(r).Frame != ReceiptRetained {
		t.Fatal("invalid ACK confirmed receipt")
	}
	if calls != 1 {
		t.Fatal("wake hint not exercised")
	}
	if err := c.WaitForReceipts(confirmationContext(t), AdmissionReceipt{}); !errors.Is(err, ErrorInvalidReceipt) {
		t.Fatalf("zero = %v", err)
	}
	foreign := confirmationClient(t)
	if err := foreign.WaitForReceipts(confirmationContext(t), r); !errors.Is(err, ErrorInvalidReceipt) {
		t.Fatalf("foreign = %v", err)
	}
	if err := c.WaitForReceipts(confirmationContext(t), r, AdmissionReceipt{}); !errors.Is(err, ErrorInvalidReceipt) {
		t.Fatalf("mixed = %v", err)
	}
	c.ab.onAck = nil
	if err := c.ab.Ack(1); err != nil {
		t.Fatal(err)
	}
	confirmationResult(t, result, nil)
	confirmationShip(t, c)
}
func TestConfirmationWrapAndAckBeforeWait(t *testing.T) {
	c := confirmationClient(t)
	c.ab.firstSeqNum, c.ab.nextSeqNum = math.MaxUint64-1, math.MaxUint64-1
	tokens := []AdmissionReceipt{confirmationShip(t, c), confirmationShip(t, c), confirmationShip(t, c)}
	for range tokens {
		c.ab.GetNextToDeliver(0)
	}
	result := confirmationPending(t, c, tokens...)
	if err := c.ab.Ack(0); err != nil {
		t.Fatal(err)
	}
	confirmationResult(t, result, nil)
	if err := c.WaitForReceipts(confirmationContext(t), tokens...); err != nil {
		t.Fatal(err)
	}
	confirmationShip(t, c)
}
func TestConfirmationCancellationCloseAndAdmissionFailure(t *testing.T) {
	t.Run("cancel then retry", func(t *testing.T) {
		c := confirmationClient(t)
		r := confirmationShip(t, c)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- c.WaitForReceipts(ctx, r) }()
		cancel()
		confirmationResult(t, done, context.Canceled)
		c.ab.GetNextToDeliver(0)
		if err := c.ab.Ack(1); err != nil {
			t.Fatal(err)
		}
		if err := c.WaitForReceipts(confirmationContext(t), r); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("close wakes", func(t *testing.T) {
		c := confirmationClient(t)
		r := confirmationShip(t, c)
		result := confirmationPending(t, c, r)
		c.Close()
		confirmationResult(t, result, ErrorClosed)
		r, err := c.ShipWithReceipt(&protocol.DataMessage{}, time.Millisecond)
		if !errors.Is(err, ErrorClosed) || r != (AdmissionReceipt{}) {
			t.Fatalf("closed admission = %v, %v", r, err)
		}
		if err := c.WaitForReceipts(confirmationContext(t)); !errors.Is(err, ErrorClosed) {
			t.Fatalf("closed empty set = %v", err)
		}
	})
	t.Run("timeout token invalid", func(t *testing.T) {
		c := confirmationClient(t)
		c.ab.UpdateCapacity(1)
		confirmationShip(t, c)
		r, err := c.ShipWithReceipt(&protocol.DataMessage{}, time.Millisecond)
		if !errors.Is(err, ErrorBufferFull) {
			t.Fatal(err)
		}
		if err := c.WaitForReceipts(confirmationContext(t), r); !errors.Is(err, ErrorInvalidReceipt) {
			t.Fatalf("failed admission = %v", err)
		}
	})
	t.Run("retired", func(t *testing.T) {
		c := confirmationClient(t)
		r, err := c.ShipWithReceiptOptions(&protocol.DataMessage{}, 0, ShipOptions{MaxTransmissions: 1})
		if err != nil {
			t.Fatal(err)
		}
		c.ab.GetNextToDeliver(0)
		result := confirmationPending(t, c, r)
		c.ab.ResetDelivery()
		confirmationResult(t, result, ErrorReceiptRetired)
	})
}
func TestConfirmationReconnect(t *testing.T) {
	c, p, _ := receiptClient(t, AckBufferOptions{})
	r := confirmationShip(t, c)
	first := p.nextFrame(t)
	result := confirmationPending(t, c, r)
	c.Reconnect()
	replay := p.nextFrame(t)
	if replay.seq != first.seq || replay.generation == first.generation {
		t.Fatal("missing reconnect replay")
	}
	receiptAck(t, c, p, replay.seq)
	confirmationResult(t, result, nil)
	confirmationShip(t, c)
}

func TestConfirmationAckBeforeAdmissionReturns(t *testing.T) {
	c, p, _ := receiptClient(t, AckBufferOptions{})
	c.receiptMu.Lock()
	locked := true
	defer func() {
		if locked {
			c.receiptMu.Unlock()
		}
	}()
	type admissionResult struct {
		receipt AdmissionReceipt
		err     error
	}
	result := make(chan admissionResult, 1)
	go func() {
		r, err := c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "synthetic"}, time.Second)
		result <- admissionResult{receipt: r, err: err}
	}()
	frame := p.nextFrame(t)
	receiptAck(t, c, p, frame.seq)
	c.receiptMu.Unlock()
	locked = false
	var admitted admissionResult
	select {
	case admitted = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("ShipWithReceipt did not return")
	}
	if admitted.err != nil {
		t.Fatal(admitted.err)
	}
	r := admitted.receipt
	if err := c.WaitForReceipts(confirmationContext(t), r); err != nil {
		t.Fatal(err)
	}
	confirmationShip(t, c)
}
