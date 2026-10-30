package uspclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/refractionPOINT/go-uspclient/protocol"
	"github.com/vmihailenco/msgpack/v5"
)

// Opt-in per-frame transmission limits (ShipWithReceiptOptions). Each test
// names the guard it covers; reverting that guard fails the assertion.

func limitBuffer(t *testing.T, o AckBufferOptions, capacity uint64) *AckBuffer {
	t.Helper()
	b, err := NewAckBuffer(o)
	if err != nil {
		t.Fatal(err)
	}
	b.UpdateCapacity(capacity)
	t.Cleanup(b.Close)
	return b
}

func limitAdmit(t *testing.T, b *AckBuffer, m *protocol.DataMessage, max uint32) *receiptState {
	t.Helper()
	state := &receiptState{}
	if err := b.addWithReceipt(m, time.Second, state, max); err != nil {
		t.Fatalf("admit: %v", err)
	}
	return state
}

func limitPlain(t *testing.T, b *AckBuffer, m *protocol.DataMessage) {
	t.Helper()
	if err := b.add(m, time.Second); err != nil {
		t.Fatalf("admit: %v", err)
	}
}

// limitDeliverAll hands out every frame the current connection would send and
// returns their sequence numbers in order.
func limitDeliverAll(b *AckBuffer) []uint64 {
	var out []uint64
	for {
		m := b.GetNextToDeliver(time.Millisecond)
		if m == nil {
			return out
		}
		out = append(out, m.SeqNum)
	}
}

func frameState(s *receiptState) ReceiptFrameState { return ReceiptFrameState(s.state.Load()) }

func countSeq(rounds [][]uint64, seq uint64) int {
	n := 0
	for _, r := range rounds {
		for _, s := range r {
			if s == seq {
				n++
			}
		}
	}
	return n
}

// Guard: the handoff count and the reset-time retirement. Limited frames are
// handed out at most MaxTransmissions times across reconnects; unlimited
// receipts and plain Ship frames keep replaying with unchanged sequences.
func TestTransmissionLimitStopsBeforeFourthHandoff(t *testing.T) {
	b := limitBuffer(t, AckBufferOptions{}, 8)
	limited := &protocol.DataMessage{TextPayload: "limited", EventType: "EMAIL_VERDICT"}
	a := limitAdmit(t, b, limited, 3)
	admitted := *limited
	limitPlain(t, b, &protocol.DataMessage{TextPayload: "plain"})
	unlimited := limitAdmit(t, b, &protocol.DataMessage{TextPayload: "receipt"}, 0)

	var rounds [][]uint64
	for round := 0; round < 6; round++ {
		if round > 0 {
			b.ResetDelivery() // what Client.Reconnect does before dialing
		}
		if round == 3 && frameState(a) != ReceiptRetired {
			t.Fatalf("not retired at the reset before the next connection: %v", frameState(a))
		}
		rounds = append(rounds, limitDeliverAll(b))
		if round == 2 && frameState(a) != ReceiptRetained {
			t.Fatalf("retired before its third handoff: %v", frameState(a))
		}
	}
	if got := countSeq(rounds, 1); got != 3 {
		t.Fatalf("limited frame handed out %d times, want 3: %v", got, rounds)
	}
	if countSeq(rounds, 2) != 6 || countSeq(rounds, 3) != 6 {
		t.Fatalf("unlimited frames must replay on every connection: %v", rounds)
	}
	for _, r := range rounds[3:] {
		if !reflect.DeepEqual(r, []uint64{2, 3}) {
			t.Fatalf("other frames' sequences changed: %v", rounds)
		}
	}
	if frameState(a) != ReceiptRetired || frameState(unlimited) != ReceiptRetained {
		t.Fatalf("states: limited=%v unlimited=%v", frameState(a), frameState(unlimited))
	}
	if !reflect.DeepEqual(admitted, *limited) {
		t.Fatal("retirement mutated the admitted message")
	}
	if got, _ := b.GetUnAcked(); len(got) != 2 || got[0].SeqNum != 2 || got[1].SeqNum != 3 {
		t.Fatalf("unacked must exclude the retired slot: %d", len(got))
	}
}

// Guard: the handoff-time fence, which holds even without a delivery reset.
func TestTransmissionLimitHandoffFence(t *testing.T) {
	b := limitBuffer(t, AckBufferOptions{}, 8)
	a := limitAdmit(t, b, &protocol.DataMessage{TextPayload: "limited"}, 1)
	limitPlain(t, b, &protocol.DataMessage{TextPayload: "plain"})
	if got := limitDeliverAll(b); !reflect.DeepEqual(got, []uint64{1, 2}) {
		t.Fatalf("first pass %v", got)
	}
	b.Lock()
	b.nextIndexToDeliver = 0
	b.deliverCursor = b.receiptHead
	b.isReadyToDeliver.Set()
	b.Unlock()
	if got := limitDeliverAll(b); !reflect.DeepEqual(got, []uint64{2}) {
		t.Fatalf("second pass handed out %v, want only the unlimited frame", got)
	}
	if frameState(a) != ReceiptRetired {
		t.Fatalf("state %v", frameState(a))
	}
}

// Guard: a cumulative ACK never turns a retired frame into an acknowledged one.
func TestTransmissionLimitAckKeepsRetired(t *testing.T) {
	b := limitBuffer(t, AckBufferOptions{}, 8)
	a := limitAdmit(t, b, &protocol.DataMessage{TextPayload: "limited"}, 1)
	c := limitAdmit(t, b, &protocol.DataMessage{TextPayload: "after"}, 0)
	limitDeliverAll(b)
	b.ResetDelivery()
	if got := limitDeliverAll(b); !reflect.DeepEqual(got, []uint64{2}) {
		t.Fatalf("replay %v", got)
	}
	// Retired at the head: released locally at once, without any ACK.
	b.RLock()
	first, free := b.firstSeqNum, b.nextIndexFree
	b.RUnlock()
	if first != 2 || free != 1 {
		t.Fatalf("retired head not released: first=%d free=%d", first, free)
	}
	if err := b.Ack(2); err != nil {
		t.Fatal(err)
	}
	if frameState(a) != ReceiptRetired || frameState(c) != ReceiptAcknowledged {
		t.Fatalf("states after ACK: retired=%v later=%v", frameState(a), frameState(c))
	}

	// Retired in the middle: the later frame's ACK removes the placeholder and
	// still must not acknowledge it.
	head := limitAdmit(t, b, &protocol.DataMessage{TextPayload: "head"}, 0)
	mid := limitAdmit(t, b, &protocol.DataMessage{TextPayload: "mid"}, 1)
	tail := limitAdmit(t, b, &protocol.DataMessage{TextPayload: "tail"}, 0)
	limitDeliverAll(b)
	b.ResetDelivery()
	if got := limitDeliverAll(b); !reflect.DeepEqual(got, []uint64{3, 5}) {
		t.Fatalf("replay with a gap %v", got)
	}
	if err := b.Ack(5); err != nil {
		t.Fatal(err)
	}
	if frameState(head) != ReceiptAcknowledged || frameState(mid) != ReceiptRetired || frameState(tail) != ReceiptAcknowledged {
		t.Fatalf("states: %v %v %v", frameState(head), frameState(mid), frameState(tail))
	}
	b.RLock()
	defer b.RUnlock()
	if b.nextIndexFree != 0 || len(b.retired) != 0 || b.receiptHead != nil || b.deliverCursor != nil {
		t.Fatalf("leftover slots=%d retired=%d", b.nextIndexFree, len(b.retired))
	}
}

// Guard: retired-head release. A retired tail is never ACKed by the peer, so it
// must not hold the window or keep Drain waiting.
func TestTransmissionLimitRetiredTailDoesNotStall(t *testing.T) {
	b := limitBuffer(t, AckBufferOptions{}, 2)
	limitPlain(t, b, &protocol.DataMessage{TextPayload: "plain"})
	a := limitAdmit(t, b, &protocol.DataMessage{TextPayload: "limited"}, 1)
	limitDeliverAll(b)
	if b.Add(&protocol.DataMessage{TextPayload: "full"}, time.Millisecond) {
		t.Fatal("window should be full")
	}
	if err := b.Ack(1); err != nil {
		t.Fatal(err)
	}
	b.ResetDelivery()
	if frameState(a) != ReceiptRetired {
		t.Fatalf("state %v", frameState(a))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b.Lock()
	b.isRunning = false // as BeginDrain
	b.Unlock()
	if err := b.drain(ctx); err != nil {
		t.Fatalf("drain stalled on a retired tail: %v", err)
	}
	b.Lock()
	b.isRunning = true
	b.Unlock()
	if !b.Add(&protocol.DataMessage{TextPayload: "room"}, time.Second) {
		t.Fatal("window not released")
	}
}

// Guard: byte charge, shared budget, escape and lastFlaggedSeq are released at
// retirement, while the placeholder still sits behind a live head.
func TestTransmissionLimitReleasesBytesEscapeAndFlag(t *testing.T) {
	p := fairPool(t, 64, 8)
	b := limitBuffer(t, AckBufferOptions{SharedBudget: p, BudgetGroup: "tenant-a", MaxBytes: 4 * testMsgSize}, 1000)
	limitPlain(t, b, textMsg(testMsgPayload)) // live, unflagged head
	fill := groupedBuffer(t, p, "tenant-a")
	for i := 0; i < 55; i++ { // the group share is 64 minus the 8-frame reserve
		if !fill.Add(flaggedTestMessage(), time.Second) {
			t.Fatal(i)
		}
	}
	big := textMsg(100 * testMsgSize)
	a := limitAdmit(t, b, big, 1) // escapes the full pool and forces an ACK request
	b.RLock()
	escaped, flagged := b.escapeBytes != 0, b.lastFlaggedSeq == 2
	b.RUnlock()
	if !escaped || !flagged {
		t.Fatalf("fixture: escape=%v flagged=%v", escaped, flagged)
	}
	limitDeliverAll(b)
	b.ResetDelivery()
	if frameState(a) != ReceiptRetired {
		t.Fatalf("state %v", frameState(a))
	}
	b.RLock()
	bytes, escape, flag, free := b.bytes, b.escapeBytes, b.lastFlaggedSeq, b.nextIndexFree
	b.RUnlock()
	if free != 2 || bytes != testMsgSize || escape != 0 || flag != 0 {
		t.Fatalf("after retire: slots=%d bytes=%d escape=%d flagged=%d", free, bytes, escape, flag)
	}
	p.mu.Lock()
	used, poolEscape := p.used, p.escape
	p.mu.Unlock()
	if used != 56*testMsgSize || poolEscape != 0 {
		t.Fatalf("shared budget not returned: used=%d escape=%d", used, poolEscape)
	}
	// With no held ACK request, the next over-limit frame is admitted and
	// flagged instead of waiting for an ACK a retired frame cannot produce.
	if !b.Add(textMsg(8*testMsgSize), 50*time.Millisecond) {
		t.Fatal("blocked on a retired frame's ACK request")
	}
}

// Guard: in drain mode the final ACK request goes on the last frame that is
// actually sent, not on a retired placeholder after it; the stored message
// stays unmodified.
func TestTransmissionLimitDrainRequestsAckOnLastLiveFrame(t *testing.T) {
	b := limitBuffer(t, AckBufferOptions{}, 8)
	live := &protocol.DataMessage{TextPayload: "live"}
	limitPlain(t, b, live)
	limitAdmit(t, b, &protocol.DataMessage{TextPayload: "limited"}, 1)
	limitDeliverAll(b)
	b.ResetDelivery()
	b.Lock()
	b.isRunning = false
	b.Unlock()
	m := b.GetNextToDeliver(time.Second)
	if m == nil || m.SeqNum != 1 || !m.AckRequested {
		t.Fatalf("final live frame must request an ACK: %+v", m)
	}
	if live.AckRequested {
		t.Fatal("retained message was mutated")
	}
	if b.GetNextToDeliver(time.Millisecond) != nil {
		t.Fatal("placeholder handed out")
	}
	if got, _ := b.GetUnAcked(); len(got) != 1 || got[0] != live {
		t.Fatalf("unacked must hold only the live message, not the placeholder: %d", len(got))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	var de *DrainError
	if err := b.drain(ctx); err == nil || !asDrainError(err, &de) || de.Unacked != 1 {
		t.Fatalf("drain error must count only the live frame: %v", err)
	}
}

func asDrainError(err error, out **DrainError) bool {
	de, ok := err.(*DrainError)
	*out = de
	return ok
}

// Guard: a limited frame acknowledged before its limit is never retired.
func TestTransmissionLimitAckBeforeLimit(t *testing.T) {
	b := limitBuffer(t, AckBufferOptions{}, 8)
	a := limitAdmit(t, b, &protocol.DataMessage{TextPayload: "limited"}, 1)
	limitDeliverAll(b)
	if err := b.Ack(1); err != nil {
		t.Fatal(err)
	}
	b.ResetDelivery()
	if frameState(a) != ReceiptAcknowledged {
		t.Fatalf("state %v", frameState(a))
	}
}

// --- real client and websocket peer -----------------------------------------

type limitFrame struct {
	generation int
	seq        uint64
	text       string
}

type limitPeer struct {
	server     *httptest.Server
	mu         sync.Mutex
	current    *websocket.Conn
	generation int
	refuse     atomic.Int32
	refused    atomic.Int32
	frames     chan limitFrame
	failed     atomic.Bool
	wg         sync.WaitGroup
}

func newLimitPeer(t *testing.T) *limitPeer {
	t.Helper()
	p := &limitPeer{frames: make(chan limitFrame, 256)}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.wg.Add(1)
		defer p.wg.Done()
		if p.refuse.Load() > 0 {
			p.refuse.Add(-1)
			p.refused.Add(1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			p.failed.Store(true)
			return
		}
		defer c.Close()
		var h protocol.ConnectionHeader
		if c.ReadJSON(&h) != nil {
			p.failed.Store(true)
			return
		}
		p.mu.Lock()
		if c.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageREADY}) != nil || c.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageFLOW, WindowSize: 8}) != nil {
			p.mu.Unlock()
			p.failed.Store(true)
			return
		}
		p.generation++
		generation := p.generation
		p.current = c
		p.mu.Unlock()
		for {
			_, raw, err := c.ReadMessage()
			if err != nil {
				return
			}
			var m protocol.DataMessage
			if msgpack.Unmarshal(raw, &m) != nil {
				p.failed.Store(true)
				return
			}
			p.frames <- limitFrame{generation: generation, seq: m.SeqNum, text: m.TextPayload}
		}
	}))
	t.Cleanup(func() { p.server.Close(); p.wg.Wait() })
	return p
}

func (p *limitPeer) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current != nil {
		p.current.Close()
		p.current = nil
	}
}

func (p *limitPeer) ack(t *testing.T, seq uint64) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil || p.current.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageACK, SeqNum: seq}) != nil {
		t.Fatal("ack write")
	}
}

// generationFrames collects frames until the given generation has sent want
// frames, then confirms nothing else arrives on it.
func (p *limitPeer) generationFrames(t *testing.T, generation, want int) []uint64 {
	t.Helper()
	var out []uint64
	deadline := time.After(10 * time.Second)
	for len(out) < want {
		select {
		case f := <-p.frames:
			if f.generation != generation {
				t.Fatalf("frame seq %d on generation %d while waiting for %d", f.seq, f.generation, generation)
			}
			out = append(out, f.seq)
		case <-deadline:
			t.Fatalf("generation %d sent %v, want %d frames", generation, out, want)
		}
	}
	select {
	case f := <-p.frames:
		t.Fatalf("extra frame seq %d on generation %d", f.seq, f.generation)
	case <-time.After(100 * time.Millisecond):
	}
	return out
}

func limitWait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(until) {
			t.Fatal("timeout: " + what)
		}
		time.Sleep(time.Millisecond)
	}
}

// End to end over real reconnects: the limited frame goes out on three
// connections only (failed dials in between do not count), the plain Ship and
// unlimited ShipWithReceipt frames go out on every connection, and the peer's
// later cumulative ACK leaves the limited receipt retired.
func TestTransmissionLimitRealReconnects(t *testing.T) {
	oldDelay := reconnectRetryDelay.Load()
	reconnectRetryDelay.Store(int64(10 * time.Millisecond))
	t.Cleanup(func() { reconnectRetryDelay.Store(oldDelay) })
	p := newLimitPeer(t)
	c, err := NewClient(context.Background(), ClientOptions{
		Identity: Identity{Oid: "00000000-0000-4000-8000-000000000001", InstallationKey: "synthetic-only"},
		DestURL:  "ws" + strings.TrimPrefix(p.server.URL, "http") + "/usp", Platform: "text",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	limitWait(t, "flow", func() bool { return c.ab.GetCurrentCapacity() == 8 })

	limitedMsg := &protocol.DataMessage{TextPayload: "limited"}
	limited, err := c.ShipWithReceiptOptions(limitedMsg, time.Second, ShipOptions{MaxTransmissions: 3})
	if err != nil {
		t.Fatal(err)
	}
	admitted := *limitedMsg
	if err := c.Ship(&protocol.DataMessage{TextPayload: "plain"}, time.Second); err != nil {
		t.Fatal(err)
	}
	unlimited, err := c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "receipt"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	all := [][]uint64{p.generationFrames(t, 1, 3)}
	for generation := 2; generation <= 5; generation++ {
		if generation == 3 {
			p.refuse.Store(2) // two failed dials before the third connection
		}
		p.drop()
		want := 3
		if generation > 3 {
			want = 2
		}
		all = append(all, p.generationFrames(t, generation, want))
		if generation == 3 && p.refused.Load() != 2 {
			t.Fatalf("fixture: refused %d dials", p.refused.Load())
		}
		if obs := c.ObserveReceipt(limited); generation <= 3 && obs.Frame != ReceiptRetained || generation > 3 && obs.Frame != ReceiptRetired {
			t.Fatalf("generation %d: limited receipt %+v", generation, obs)
		}
	}
	if got := countSeq(all, 1); got != 3 {
		t.Fatalf("limited frame sent %d times, want 3: %v", got, all)
	}
	if countSeq(all, 2) != 5 || countSeq(all, 3) != 5 {
		t.Fatalf("unlimited frames must be sent on every connection: %v", all)
	}
	p.ack(t, 3)
	limitWait(t, "ack", func() bool { return c.ObserveReceipt(unlimited).Frame == ReceiptAcknowledged })
	if obs := c.ObserveReceipt(limited); obs != (ReceiptObservation{Frame: ReceiptRetired, Lifetime: ReceiptLive}) {
		t.Fatalf("cumulative ACK changed the retired receipt: %+v", obs)
	}
	if unsent, _ := c.GetUnsent(); len(unsent) != 0 {
		t.Fatalf("unsent %d", len(unsent))
	}
	if !reflect.DeepEqual(admitted, *limitedMsg) {
		t.Fatal("admitted message mutated")
	}
	if tail, _ := c.Close(); len(tail) != 0 {
		t.Fatalf("close returned %d", len(tail))
	}
	if obs := c.ObserveReceipt(limited); obs.Frame != ReceiptRetired || obs.Lifetime != ReceiptClosed {
		t.Fatalf("after close %+v", obs)
	}
	if p.failed.Load() {
		t.Fatal("peer fixture failed")
	}
}

// Guard: a retired placeholder is never handed to a sender, even if the
// delivery cursor were out of step with the slot (defense in depth: a
// placeholder on the wire would be an empty event).
func TestTransmissionLimitPlaceholderNeverSent(t *testing.T) {
	b := limitBuffer(t, AckBufferOptions{}, 8)
	limitPlain(t, b, &protocol.DataMessage{TextPayload: "head"})
	limitAdmit(t, b, &protocol.DataMessage{TextPayload: "limited"}, 1)
	limitPlain(t, b, &protocol.DataMessage{TextPayload: "tail"})
	limitDeliverAll(b)
	b.ResetDelivery()
	b.Lock()
	b.deliverCursor = nil
	b.Unlock()
	if got := limitDeliverAll(b); !reflect.DeepEqual(got, []uint64{1, 3}) {
		t.Fatalf("handed out %v", got)
	}
}
