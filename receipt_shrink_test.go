package uspclient

import (
	"errors"
	"testing"
	"time"

	"github.com/refractionPOINT/go-uspclient/protocol"
)

func TestReceiptSurvivesSmallerReconnectWindowAndClose(t *testing.T) {
	pool := NewByteBudget(1 << 20)
	b, err := NewAckBuffer(AckBufferOptions{SharedBudget: pool})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	b.UpdateCapacity(100000)
	c := &Client{ab: b}
	var receipts []*Receipt
	var charge uint64
	for range 4 {
		message := &protocol.DataMessage{TextPayload: "tracked-reconnect-tail"}
		charge = estimateMessageSize(message) + receiptOverheadBytes
		receipt, err := c.ShipWithReceipt(message, time.Second)
		if err != nil || b.GetNextToDeliver(0) != message {
			t.Fatalf("receipt admission/delivery: %v", err)
		}
		receipts = append(receipts, receipt)
	}
	b.UpdateCapacity(1)
	b.ResetDelivery()
	if len(b.buff) != 4 || cap(b.sizes) != 4 || pool.Used() != 4*charge || len(b.receipts) != 4 {
		t.Fatalf("reconnect lost receipt ownership or retained old window: slots=%d sizes=%d bytes=%d receipts=%d", len(b.buff), cap(b.sizes), pool.Used(), len(b.receipts))
	}
	for seq := uint64(1); seq <= 4; seq++ {
		receiptPending(t, receipts[seq-1])
		if message := b.GetNextToDeliver(0); message == nil || message.SeqNum != seq {
			t.Fatalf("replay %d: %v", seq, message)
		}
	}
	if err := b.Ack(3); err != nil {
		t.Fatal(err)
	}
	for _, receipt := range receipts[:3] {
		receiptResult(t, receipt, nil)
	}
	receiptPending(t, receipts[3])
	if len(b.buff) != 1 || cap(b.sizes) != 1 || pool.Used() != charge || len(b.receipts) != 1 {
		t.Fatalf("partial ACK did not reclaim only confirmed ownership: slots=%d sizes=%d bytes=%d receipts=%d", len(b.buff), cap(b.sizes), pool.Used(), len(b.receipts))
	}
	b.UpdateCapacity(0)
	if receipt, err := c.ShipWithReceipt(&protocol.DataMessage{}, time.Millisecond); receipt != nil || !errors.Is(err, ErrorBufferFull) {
		t.Fatalf("zero FLOW admitted receipt: %v, %v", receipt, err)
	}
	b.Close()
	receiptResult(t, receipts[3], ErrorClosed)
	tail, err := b.GetUnAcked()
	if err != nil || len(tail) != 1 || tail[0].SeqNum != 4 || pool.Used() != 0 || len(b.receipts) != 0 {
		t.Fatalf("Close lost unconfirmed ownership: tail=%v err=%v bytes=%d receipts=%d", tail, err, pool.Used(), len(b.receipts))
	}
}
