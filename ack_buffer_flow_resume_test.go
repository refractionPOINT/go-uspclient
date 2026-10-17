package uspclient

import (
	"testing"
	"time"

	"github.com/refractionPOINT/go-uspclient/protocol"
)

func TestAckBufferPartialAckThenFlowResumeReopensAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		paused, acked, resumed uint64
	}{
		{"zero-flow", 0, 3, 2},
		{"overfull-small-window", 1, 2, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := NewByteBudget(1 << 20)
			b, err := NewAckBuffer(AckBufferOptions{SharedBudget: pool})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(b.Close)
			b.UpdateCapacity(100000)
			message := &protocol.DataMessage{TextPayload: "retained"}
			charge := estimateMessageSize(message)
			for range 4 {
				copy := *message
				if !b.Add(&copy, time.Second) || b.GetNextToDeliver(0) != &copy {
					t.Fatal("initial message was not admitted and delivered")
				}
			}
			b.UpdateCapacity(tc.paused)
			if err := b.Ack(tc.acked); err != nil {
				t.Fatal(err)
			}
			pending := uint64(4) - tc.acked
			if b.isAvailable.IsSet() || pool.Used() != pending*charge {
				t.Fatal("partial ACK violated reduced FLOW or pending byte ownership")
			}
			b.UpdateCapacity(tc.resumed)
			if !b.isAvailable.IsSet() {
				t.Fatalf("FLOW resume left admission closed: pending=%d FLOW=%d backing=%d", pending, tc.resumed, len(b.buff))
			}
			if pool.Used() != pending*charge || len(b.buff) != int(tc.resumed) || cap(b.sizes) != int(tc.resumed) {
				t.Fatal("FLOW resume changed byte ownership or retained oversized backing")
			}
			if !b.Add(&protocol.DataMessage{TextPayload: "after-resume"}, time.Second) {
				t.Fatal("FLOW resume did not admit the available slot")
			}
			if b.isAvailable.IsSet() {
				t.Fatal("admission stayed open after filling the resumed window")
			}
			if next := b.GetNextToDeliver(0); next == nil || next.SeqNum != 5 {
				t.Fatalf("resumed message sequence: %v", next)
			}
			if err := b.Ack(5); err != nil {
				t.Fatal(err)
			}
			if pool.Used() != 0 || b.BufferedBytes() != 0 {
				t.Fatal("final confirmation retained bytes")
			}
		})
	}
}
