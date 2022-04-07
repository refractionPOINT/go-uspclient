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
