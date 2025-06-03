package uspclient

import (
	"math"
	"testing"
	"time"

	"github.com/refractionPOINT/go-uspclient/protocol"
)

func TestAckBufferBasics(t *testing.T) {
	b, err := NewAckBuffer(AckBufferOptions{})
	b.UpdateCapacity(5)
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	if b.isReadyToDeliver.IsSet() {
		t.Error("should not be ready to deliver")
	}
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("unexpected failed add")
	}
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("unexpected failed add")
	}
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("unexpected failed add")
	}

	if !b.isAvailable.IsSet() {
		t.Error("should still be available space in buffer")
	}
	if !b.isReadyToDeliver.IsSet() {
		t.Error("should be ready to deliver")
	}

	// Cannot ack if not delivered.
	if err := b.Ack(2); err == nil {
		t.Errorf("ack should have failed because of delivery: %v", err)
	}

	// Deliver 2.
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}

	// Still cannot ack all, 1 not delivered.
	if err := b.Ack(3); err == nil {
		t.Error("ack should have failed because of delivery")
	}
	// But can ack 2 that have been delivered.
	if err := b.Ack(2); err != nil {
		t.Errorf("ack should succeeded: %v", err)
	}

	// One undelivered message in buffer.

	// Deliver 1.
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}
	// Fail to deliver one more, nothing in buffer.
	if b.isReadyToDeliver.IsSet() {
		t.Error("should not be ready to deliver")
	}
	if b.GetNextToDeliver(0) != nil {
		t.Error("should not have a message to deliver")
	}
	// Try to ack old message, like an ack re-delivery.
	if err := b.Ack(2); err == nil {
		t.Errorf("ack should not succeed on old sequence number: %v", err)
	}
	// Try to ack in the future.
	if err := b.Ack(4); err == nil {
		t.Errorf("ack should not succeed on future sequence number: %v", err)
	}
	// Ack the last message in buffer.
	if err := b.Ack(3); err != nil {
		t.Errorf("ack should succeeded: %v", err)
	}
	if b.isReadyToDeliver.IsSet() {
		t.Error("should not be ready to deliver")
	}
}

func TestAckBufferWaits(t *testing.T) {
	b, err := NewAckBuffer(AckBufferOptions{})
	b.UpdateCapacity(5)
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}

	go func() {
		time.Sleep(2 * time.Second)
		if !b.Add(&protocol.DataMessage{}, 0) {
			t.Errorf("failed to add")
		}
	}()

	if b.GetNextToDeliver(5*time.Second) == nil {
		t.Error("expected a delayed delivery")
	}

	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add")
	}
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add")
	}

	go func() {
		time.Sleep(2 * time.Second)
		if err := b.Ack(1); err != nil {
			t.Errorf("expected successful ack: %v", err)
		}
	}()

	if !b.Add(&protocol.DataMessage{}, 5*time.Second) {
		t.Error("expected a delayed add")
	}
}

func TestAckBufferBoundaries(t *testing.T) {
	// Case 1: overflow ack between bounds
	b, err := NewAckBuffer(AckBufferOptions{})
	b.UpdateCapacity(10)
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.firstSeqNum = math.MaxUint64 - 3
	b.nextSeqNum = math.MaxUint64 - 3

	for i := 0; i < 10; i++ {
		if !b.Add(&protocol.DataMessage{}, 0) {
			t.Error("failed to add")
		}
	}

	for i := 0; i < len(b.buff)-2; i++ {
		if b.GetNextToDeliver(0) == nil {
			t.Error("should have been able to get a delivery")
		}
	}

	if err := b.Ack(b.nextSeqNum - 3); err != nil {
		t.Errorf("failed acking: %v", err)
	}

	out, err := b.GetUnAcked()
	if err != nil {
		t.Errorf("failed getting unacked: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("unexpected number of unacked events: %d", len(out))
	}

	// Case 2: overflow ack on exact bounds
	b, err = NewAckBuffer(AckBufferOptions{})
	b.UpdateCapacity(10)
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.firstSeqNum = math.MaxUint64 - 3
	b.nextSeqNum = math.MaxUint64 - 3

	for i := 0; i < 10; i++ {
		if !b.Add(&protocol.DataMessage{}, 0) {
			t.Error("failed to add")
		}
	}

	for i := 0; i < len(b.buff); i++ {
		if b.GetNextToDeliver(0) == nil {
			t.Error("should have been able to get a delivery")
		}
	}

	if err := b.Ack(b.nextSeqNum - 1); err != nil {
		t.Errorf("failed acking: %v", err)
	}

	out, err = b.GetUnAcked()
	if err != nil {
		t.Errorf("failed getting unacked: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("unexpected number of unacked events: %d", len(out))
	}

	// Case 3: overflow ack out of bounds
	b, err = NewAckBuffer(AckBufferOptions{})
	b.UpdateCapacity(10)
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.firstSeqNum = math.MaxUint64 - 3
	b.nextSeqNum = math.MaxUint64 - 3

	for i := 0; i < 10; i++ {
		if !b.Add(&protocol.DataMessage{}, 0) {
			t.Error("failed to add")
		}
	}

	if err := b.Ack(b.nextSeqNum + 2); err == nil {
		t.Errorf("expected a failed acking: %v (%+v)", err, b)
	}

	out, err = b.GetUnAcked()
	if err != nil {
		t.Errorf("failed getting unacked: %v", err)
	}
	if len(out) != 10 {
		t.Errorf("unexpected number of unacked events: %+v", out)
	}

	// Case 4: ack overflow
	b, err = NewAckBuffer(AckBufferOptions{})
	b.UpdateCapacity(3)
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}

	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add")
	}
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add")
	}
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add")
	}

	if b.GetNextToDeliver(0) == nil {
		t.Error("should have been able to get a delivery")
	}
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have been able to get a delivery")
	}
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have been able to get a delivery")
	}

	if err := b.Ack(4); err == nil {
		t.Errorf("invalid ack should have generated an error")
	}
}

func TestAckAlways(t *testing.T) {
	// Case 1: overflow ack between bounds
	b, err := NewAckBuffer(AckBufferOptions{})
	b.UpdateCapacity(10)
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}

	seqNum := uint64(1)
	for i := 0; i < 100; i++ {
		if !b.Add(&protocol.DataMessage{}, 0) {
			t.Error("failed to add")
			return
		}
		if b.GetNextToDeliver(0) == nil {
			t.Error("should have been able to get a delivery")
			return
		}
		if err := b.Ack(seqNum); err != nil {
			t.Errorf("failed acking: %v", err)
			return
		}
		seqNum++
	}

	out, err := b.GetUnAcked()
	if err != nil {
		t.Errorf("failed getting unacked: %v", err)
		return
	}
	if len(out) != 0 {
		t.Errorf("unexpected number of unacked events: %d", len(out))
	}
}

func TestAckBufferScaleUpDown(t *testing.T) {
	b, _ := NewAckBuffer(AckBufferOptions{})
	b.UpdateCapacity(5)
	if !b.Add(&protocol.DataMessage{}, 1*time.Second) {
		t.Error("unexpected failed add")
	}
	if !b.Add(&protocol.DataMessage{}, 1*time.Second) {
		t.Error("unexpected failed add")
	}
	if !b.Add(&protocol.DataMessage{}, 1*time.Second) {
		t.Error("unexpected failed add")
	}
	if !b.Add(&protocol.DataMessage{}, 1*time.Second) {
		t.Error("unexpected failed add")
	}
	if !b.Add(&protocol.DataMessage{}, 1*time.Second) {
		t.Error("unexpected failed add")
	}

	b.UpdateCapacity(7)

	if !b.isAvailable.IsSet() {
		t.Error("should still be available space in buffer")
	}
	if !b.isReadyToDeliver.IsSet() {
		t.Error("should be ready to deliver")
	}

	if !b.Add(&protocol.DataMessage{}, 1*time.Second) {
		t.Error("unexpected failed add")
	}
	if !b.Add(&protocol.DataMessage{}, 1*time.Second) {
		t.Error("unexpected failed add")
	}

	if b.Add(&protocol.DataMessage{}, 1*time.Second) {
		t.Error("unexpected add")
	}

	if b.isAvailable.IsSet() {
		t.Error("should not be available space in buffer")
	}
	if !b.isReadyToDeliver.IsSet() {
		t.Error("should be ready to deliver")
	}

	b.UpdateCapacity(4)

	if b.isAvailable.IsSet() {
		t.Error("should not be available space in buffer")
	}
	if !b.isReadyToDeliver.IsSet() {
		t.Error("should be ready to deliver")
	}

	if b.Add(&protocol.DataMessage{}, 1*time.Second) {
		t.Error("unexpected add")
	}

	if b.isAvailable.IsSet() {
		t.Error("should not be available space in buffer")
	}
	if !b.isReadyToDeliver.IsSet() {
		t.Error("should be ready to deliver")
	}

	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}

	if b.isAvailable.IsSet() {
		t.Error("should not be available space in buffer")
	}
	if !b.isReadyToDeliver.IsSet() {
		t.Error("should be ready to deliver")
	}

	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}

	if b.isAvailable.IsSet() {
		t.Error("should not be available space in buffer")
	}
	if !b.isReadyToDeliver.IsSet() {
		t.Error("should be ready to deliver")
	}

	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}

	if b.isAvailable.IsSet() {
		t.Error("should not be available space in buffer")
	}
	if !b.isReadyToDeliver.IsSet() {
		t.Error("should be ready to deliver")
	}

	if err := b.Ack(4); err != nil {
		t.Errorf("failed acking: %v", err)
	}

	if !b.isAvailable.IsSet() {
		t.Error("should still be available space in buffer")
	}
	if !b.isReadyToDeliver.IsSet() {
		t.Error("should be ready to deliver")
	}

	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}

	if b.GetNextToDeliver(0) != nil {
		t.Error("should not have a message to deliver")
	}
}

func TestAckBufferInitialAck(t *testing.T) {
	b, _ := NewAckBuffer(AckBufferOptions{})
	if !b.Add(&protocol.DataMessage{}, 1*time.Second) {
		t.Error("unexpected failed add")
	}
	if b.Add(&protocol.DataMessage{}, 1*time.Second) {
		t.Error("unexpected add")
	}

	if b.isAvailable.IsSet() {
		t.Error("should not be available space in buffer")
	}
	if !b.isReadyToDeliver.IsSet() {
		t.Error("should be ready to deliver")
	}

	if msg := b.GetNextToDeliver(0); msg == nil {
		t.Error("should have a message to deliver")
	} else if !msg.AckRequested {
		t.Errorf("missing ack request: %+v", msg)
	}

	if b.isAvailable.IsSet() {
		t.Error("should not be available space in buffer")
	}
	if b.isReadyToDeliver.IsSet() {
		t.Error("should not be ready to deliver")
	}

	if err := b.Ack(1); err != nil {
		t.Errorf("failed acking: %v", err)
	}

	if !b.isAvailable.IsSet() {
		t.Error("should still be available space in buffer")
	}
	if b.isReadyToDeliver.IsSet() {
		t.Error("should not be ready to deliver")
	}
}

func TestAckBufferCallbacks(t *testing.T) {
	backPressureCalled := false
	ackCalled := false

	b, err := NewAckBuffer(AckBufferOptions{
		OnBackPressure: func() { backPressureCalled = true },
		OnAck:          func() { ackCalled = true },
	})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.UpdateCapacity(2)

	// Fill buffer
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add first message")
	}
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add second message")
	}

	// Try to add one more - should trigger back pressure
	if b.Add(&protocol.DataMessage{}, 1*time.Millisecond) {
		t.Error("should not have been able to add message")
	}
	if !backPressureCalled {
		t.Error("back pressure callback should have been called")
	}

	// Deliver and ack first message
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}
	if err := b.Ack(1); err != nil {
		t.Errorf("failed to ack: %v", err)
	}
	if !ackCalled {
		t.Error("ack callback should have been called")
	}
}

func TestAckBufferClose(t *testing.T) {
	b, err := NewAckBuffer(AckBufferOptions{})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}

	// Close the buffer
	b.Close()

	// Try to add message - should fail
	if b.Add(&protocol.DataMessage{}, 0) {
		t.Error("should not have been able to add message after close")
	}

	// Try to get next message - should return nil
	if b.GetNextToDeliver(0) != nil {
		t.Error("should not have been able to get message after close")
	}
}

func TestAckBufferResetDelivery(t *testing.T) {
	b, err := NewAckBuffer(AckBufferOptions{})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.UpdateCapacity(3)

	// Add messages
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add first message")
	}
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add second message")
	}

	// Deliver first message
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}

	// Reset delivery
	b.ResetDelivery()

	// Should be able to deliver first message again
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver after reset")
	}
}

func TestAckBufferTimeouts(t *testing.T) {
	b, err := NewAckBuffer(AckBufferOptions{})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.UpdateCapacity(1)

	// Fill buffer
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add message")
	}

	// Try to add with timeout - should fail
	if b.Add(&protocol.DataMessage{}, 100*time.Millisecond) {
		t.Error("should not have been able to add message with timeout")
	}

	// Try to get next with timeout - should succeed
	if b.GetNextToDeliver(100*time.Millisecond) == nil {
		t.Error("should have been able to get message with timeout")
	}

	// Try to get next with timeout - should fail
	if b.GetNextToDeliver(100*time.Millisecond) != nil {
		t.Error("should not have been able to get message with timeout")
	}
}

func TestAckBufferSequenceWrapping(t *testing.T) {
	b, err := NewAckBuffer(AckBufferOptions{})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.UpdateCapacity(3)

	// Set sequence numbers near max uint64
	b.firstSeqNum = math.MaxUint64 - 2
	b.nextSeqNum = math.MaxUint64 - 2

	// Add messages
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add first message")
	}
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add second message")
	}
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add third message")
	}

	// Deliver and ack first message
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}
	if err := b.Ack(math.MaxUint64 - 2); err != nil {
		t.Errorf("failed to ack: %v", err)
	}

	// Try to ack with sequence number that would wrap incorrectly
	if err := b.Ack(0); err == nil {
		t.Error("should have failed to ack with wrapping sequence number")
	}

	// Deliver and ack remaining messages
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}
	if b.GetNextToDeliver(0) == nil {
		t.Error("should have a message to deliver")
	}
	if err := b.Ack(math.MaxUint64); err != nil {
		t.Errorf("failed to ack: %v", err)
	}
}

func TestAckBufferBoundaryConditions(t *testing.T) {
	// Test with minimum capacity
	b, err := NewAckBuffer(AckBufferOptions{})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.UpdateCapacity(1)

	// Test adding message at capacity
	if !b.Add(&protocol.DataMessage{}, 0) {
		t.Error("failed to add message at capacity")
	}
	if b.Add(&protocol.DataMessage{}, 1*time.Millisecond) {
		t.Error("should not have been able to add message beyond capacity")
	}

	// Test with zero capacity
	b.UpdateCapacity(0)
	if b.Add(&protocol.DataMessage{}, 1*time.Millisecond) {
		t.Error("should not have been able to add message with zero capacity")
	}

	// Test with very large capacity
	b.UpdateCapacity(1024)
	if !b.Add(&protocol.DataMessage{}, 1*time.Millisecond) {
		t.Error("failed to add message with large capacity")
	}

	// Test with nil message
	if b.Add(nil, 1*time.Millisecond) {
		t.Error("should not have been able to add nil message")
	}
}

func TestAckBufferRaceConditions(t *testing.T) {
	b, err := NewAckBuffer(AckBufferOptions{})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.UpdateCapacity(100)

	// Test concurrent adds
	addDone := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 10; j++ {
				b.Add(&protocol.DataMessage{}, 0)
			}
			addDone <- true
		}()
	}
	for i := 0; i < 10; i++ {
		<-addDone
	}

	// Test concurrent delivery and acks
	deliverDone := make(chan bool)
	for i := 0; i < 5; i++ {
		go func() {
			for j := 0; j < 20; j++ {
				if msg := b.GetNextToDeliver(0); msg != nil {
					b.Ack(msg.SeqNum)
				}
			}
			deliverDone <- true
		}()
	}
	for i := 0; i < 5; i++ {
		<-deliverDone
	}

	// Test concurrent capacity updates
	updateDone := make(chan bool)
	for i := 0; i < 5; i++ {
		go func() {
			for j := 0; j < 10; j++ {
				b.UpdateCapacity(uint64(50 + j))
			}
			updateDone <- true
		}()
	}
	for i := 0; i < 5; i++ {
		<-updateDone
	}

	// Verify final state
	out, err := b.GetUnAcked()
	if err != nil {
		t.Errorf("failed getting unacked: %v", err)
	}
	if len(out) > 100 {
		t.Errorf("unexpected number of unacked events: %d", len(out))
	}
}

func TestAckBufferStressTest(t *testing.T) {
	b, err := NewAckBuffer(AckBufferOptions{})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.UpdateCapacity(1000)

	// Rapid add/deliver/ack cycle
	for i := 0; i < 10000; i++ {
		if !b.Add(&protocol.DataMessage{}, 0) {
			t.Errorf("failed to add message at iteration %d", i)
			return
		}
		if msg := b.GetNextToDeliver(0); msg == nil {
			t.Errorf("failed to get message at iteration %d", i)
			return
		} else {
			if err := b.Ack(msg.SeqNum); err != nil {
				t.Errorf("failed to ack message at iteration %d: %v", i, err)
				return
			}
		}
	}

	// Verify final state
	out, err := b.GetUnAcked()
	if err != nil {
		t.Errorf("failed getting unacked: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("unexpected number of unacked events: %d", len(out))
	}
}

func TestAckBufferConcurrentClose(t *testing.T) {
	b, err := NewAckBuffer(AckBufferOptions{})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.UpdateCapacity(100)

	// Start multiple goroutines that will be interrupted by close
	done := make(chan bool)
	for i := 0; i < 5; i++ {
		go func() {
			for {
				b.Add(&protocol.DataMessage{}, 0)
				b.GetNextToDeliver(0)
				select {
				case <-done:
					return
				default:
				}
			}
		}()
	}

	// Close the buffer
	b.Close()

	// Signal goroutines to stop
	close(done)

	// Verify operations fail after close
	if b.Add(&protocol.DataMessage{}, 0) {
		t.Error("should not have been able to add message after close")
	}
	if b.GetNextToDeliver(0) != nil {
		t.Error("should not have been able to get message after close")
	}
}

func TestAckBufferResetDeliveryWithAcks(t *testing.T) {
	b, err := NewAckBuffer(AckBufferOptions{})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.UpdateCapacity(5)

	// Add messages 1-5
	for i := 0; i < 5; i++ {
		if !b.Add(&protocol.DataMessage{}, 0) {
			t.Errorf("failed to add message %d", i+1)
		}
	}

	// Deliver messages 1-3
	for i := 0; i < 3; i++ {
		if b.GetNextToDeliver(0) == nil {
			t.Errorf("should have a message to deliver at %d", i+1)
		}
	}

	// Ack messages 1-2 (this advances firstSeqNum to 3)
	for i := 1; i <= 2; i++ {
		if err := b.Ack(uint64(i)); err != nil {
			t.Errorf("failed to ack message %d: %v", i, err)
		}
	}

	// At this point:
	// - firstSeqNum = 3 (after acking seq 2)
	// - nextIndexToDeliver = 1 (one message delivered but not yet acked)
	// - Buffer contains messages 3-5

	// Reset delivery (simulating reconnection)
	b.ResetDelivery()

	// Now nextIndexToDeliver = 0, but firstSeqNum is still 3
	// This should work after our fix - we can ACK message 3 even though
	// it's considered "undelivered" after ResetDelivery
	if err := b.Ack(3); err != nil {
		t.Errorf("should be able to ack message 3 after ResetDelivery, but got error: %v", err)
	}
}
