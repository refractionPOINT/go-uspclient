package uspclient

import (
	"math"
	"testing"
	"time"
)

func TestAckBuffer(t *testing.T) {
	capacity := uint64(100)
	testMessages := []*UspDataMessage{}
	for i := uint64(0); i < capacity; i++ {
		testMessages = append(testMessages, &UspDataMessage{})
	}
	b, err := NewAckBuffer(AckBufferOptions{
		BufferCapacity: capacity,
	})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	for i := 0; i < len(testMessages); i++ {
		if !b.Add(testMessages[i], 0) {
			t.Error("unexpected failed add")
		}
	}

	if b.Add(&UspDataMessage{}, 0) {
		t.Error("expected a failed add")
	}

	go func() {
		time.Sleep(2 * time.Second)
		if err := b.Ack(3); err != nil {
			t.Errorf("failed acking: %v", err)
		}
	}()

	if !b.Add(&UspDataMessage{}, 30*time.Second) {
		t.Error("expected a delayed add")
	}
	if !b.Add(&UspDataMessage{}, 0) {
		t.Error("expected a direct add")
	}
	if !b.Add(&UspDataMessage{}, 0) {
		t.Error("expected a direct add")
	}
	if b.Add(&UspDataMessage{}, 0) {
		t.Error("expected a failed add")
	}

	if err := b.Ack(capacity); err != nil {
		t.Errorf("failed acking: %v", err)
	}

	out, err := b.GetUnAcked()
	if err != nil {
		t.Errorf("failed getting unacked: %v", err)
	}
	if len(out) != 3 {
		t.Errorf("unexpected number of unacked events: %+v", out)
	}
}

func TestAckBufferBoundaries(t *testing.T) {
	// Case 1: overflow ack between bounds
	b, err := NewAckBuffer(AckBufferOptions{
		BufferCapacity: 10,
	})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.firstSeqNum = math.MaxUint64 - 3
	b.nextSeqNum = math.MaxUint64 - 3

	for i := 0; i < 10; i++ {
		if !b.Add(&UspDataMessage{}, 0) {
			t.Error("failed to add")
		}
	}

	if err := b.Ack(b.nextSeqNum - 3); err != nil {
		t.Errorf("failed acking: %v (%+v)", err, b)
	}

	out, err := b.GetUnAcked()
	if err != nil {
		t.Errorf("failed getting unacked: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("unexpected number of unacked events: %+v", out)
	}

	// Case 2: overflow ack on exact bounds
	b, err = NewAckBuffer(AckBufferOptions{
		BufferCapacity: 10,
	})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.firstSeqNum = math.MaxUint64 - 3
	b.nextSeqNum = math.MaxUint64 - 3

	for i := 0; i < 10; i++ {
		if !b.Add(&UspDataMessage{}, 0) {
			t.Error("failed to add")
		}
	}

	if err := b.Ack(b.nextSeqNum - 1); err != nil {
		t.Errorf("failed acking: %v (%+v)", err, b)
	}

	out, err = b.GetUnAcked()
	if err != nil {
		t.Errorf("failed getting unacked: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("unexpected number of unacked events: %+v", out)
	}

	// Case 3: overflow ack out of bounds
	b, err = NewAckBuffer(AckBufferOptions{
		BufferCapacity: 10,
	})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	b.firstSeqNum = math.MaxUint64 - 3
	b.nextSeqNum = math.MaxUint64 - 3

	for i := 0; i < 10; i++ {
		if !b.Add(&UspDataMessage{}, 0) {
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
