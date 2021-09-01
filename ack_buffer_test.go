package uspclient

import "testing"

func TestAckBuffer(t *testing.T) {
	capacity := uint64(100)
	b, err := NewAckBuffer(AckBufferOptions{
		BufferCapacity: capacity,
	})
	if err != nil {
		t.Errorf("failed creating ack buffer: %v", err)
		return
	}
	for i := uint64(0); i < capacity; i++ {
		if !b.Add(&UspDataMessage{}, 0) {
			t.Error("unexpected failed add")
		}
	}

	if b.Add(&UspDataMessage{}, 0) {
		t.Error("expected a failed add")
	}
}
