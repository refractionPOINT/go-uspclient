package uspclient

import (
	"sync"
	"testing"
	"time"

	"github.com/refractionPOINT/go-uspclient/protocol"
)

// Minimal common surface of the PR and master AckBuffer.
type abuf interface {
	Add(*protocol.DataMessage, time.Duration) bool
	GetNextToDeliver(time.Duration) *protocol.DataMessage
	Ack(uint64) error
	Close()
}

func flagged(n int) *protocol.DataMessage {
	m := textMsg(n)
	m.AckRequested = true
	return m
}

// Ungrouped NewByteBudget: client X holds an unacknowledged flagged message
// (its connection is stalled) and blocks on a message that cannot fit while
// it holds that message. Other clients each hold one in-flight flagged frame
// (so they have an ACK coming) and add one small frame.
//
// master: the other clients use the free room immediately.
// A FIFO/escrow regression makes X the FIFO head of the single legacy group, its
// partial credit absorbs all free room, and every other client must queue
// behind it until X's own ACK arrives. Legacy callers must match master.
func legacyHOL(t *testing.T, newBuf func(func()) abuf, used func() uint64) (admitted int) {
	const T = 100 * testMsgSize
	blocked := make(chan struct{})
	var once sync.Once
	x := newBuf(func() { once.Do(func() { close(blocked) }) })
	if !x.Add(flagged(30*testMsgSize-messageOverheadBytes), time.Second) {
		t.Fatal("x first")
	}
	if x.GetNextToDeliver(time.Second) == nil { // delivered, never acked
		t.Fatal("deliver x")
	}
	xDone := make(chan bool, 1)
	go func() { xDone <- x.Add(flagged(80*testMsgSize-messageOverheadBytes), 3*time.Second) }()
	t.Cleanup(func() { x.Close(); <-xDone })
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("large producer did not reach shared budget wait")
	}
	others := make([]abuf, 5)
	for i := range others {
		others[i] = newBuf(nil)
		t.Cleanup(others[i].Close)
		if !others[i].Add(flagged(testMsgPayload), time.Second) { // escape or fit
			t.Fatal("other first")
		}
		if others[i].GetNextToDeliver(time.Second) == nil {
			t.Fatal("deliver other")
		}
	}
	for _, o := range others {
		if o.Add(textMsg(testMsgPayload), 200*time.Millisecond) {
			admitted++
		}
	}
	t.Logf("used=%d of %d; other clients admitted %d/5 small frames while X waits", used(), uint64(T), admitted)
	return admitted
}

func TestLegacyByteBudgetLargeWaiterDoesNotBlockOtherClients(t *testing.T) {
	const T = 100 * testMsgSize
	t.Run("legacy", func(t *testing.T) {
		p := NewByteBudget(T)
		got := legacyHOL(t, func(onBackPressure func()) abuf {
			b, _ := NewAckBuffer(AckBufferOptions{SharedBudget: p, OnBackPressure: onBackPressure})
			b.UpdateCapacity(1000)
			return b
		}, p.Used)
		if got != 5 {
			t.Errorf("legacy path admitted %d/5 where master admitted 5: head-of-line blocking behind a partially credited waiter", got)
		} else if got := p.Used(); got != 40*testMsgSize {
			t.Errorf("legacy usage %d, want only 40 held frames, without escrow", got)
		}
	})
}
