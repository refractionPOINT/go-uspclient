package uspclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
	"github.com/refractionPOINT/go-uspclient/protocol"
	"github.com/vmihailenco/msgpack/v5"
)

type receiptPeerFrame struct {
	seq        uint64
	generation int
}
type receiptPeer struct {
	server  *httptest.Server
	mu      sync.Mutex
	current *websocket.Conn
	conns   []*websocket.Conn
	wg      sync.WaitGroup
	frames  chan receiptPeerFrame
	closed  chan struct{}
	next    int
	failed  atomic.Bool
	warned  atomic.Int32
}

func newReceiptPeer(t *testing.T) *receiptPeer {
	t.Helper()
	p := &receiptPeer{frames: make(chan receiptPeerFrame, 32), closed: make(chan struct{}, 8)}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.wg.Add(1)
		defer p.wg.Done()
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			p.failed.Store(true)
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		var h protocol.ConnectionHeader
		if c.ReadJSON(&h) != nil || !h.IsCompressed {
			p.failed.Store(true)
			return
		}
		if c.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageREADY}) != nil || c.WriteJSON(protocol.ControlMessage{Verb: protocol.ControlMessageFLOW, WindowSize: 4}) != nil {
			p.failed.Store(true)
			return
		}
		p.mu.Lock()
		p.next++
		generation := p.next
		p.current = c
		p.conns = append(p.conns, c)
		p.mu.Unlock()
		defer func() { p.closed <- struct{}{} }()
		for {
			_, raw, err := c.ReadMessage()
			if err != nil {
				return
			}
			z, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				p.failed.Store(true)
				return
			}
			var frame protocol.DataMessage
			err = msgpack.NewDecoder(z).Decode(&frame)
			z.Close()
			if err != nil {
				p.failed.Store(true)
				return
			}
			select {
			case p.frames <- receiptPeerFrame{seq: frame.SeqNum, generation: generation}:
			case <-time.After(10 * time.Second):
				p.failed.Store(true)
				return
			}
		}
	}))
	return p
}
func (p *receiptPeer) control(t *testing.T, msg protocol.ControlMessage) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		t.Fatal("receipt_fixture_no_peer")
	}
	if p.current.WriteJSON(msg) != nil {
		t.Fatal("receipt_fixture_control_write")
	}
}
func (p *receiptPeer) nextFrame(t *testing.T) receiptPeerFrame {
	t.Helper()
	select {
	case f := <-p.frames:
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("receipt_fixture_frame_timeout")
		return receiptPeerFrame{}
	}
}
func (p *receiptPeer) close() {
	p.mu.Lock()
	for _, c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.server.Close()
	p.wg.Wait()
}
func receiptWait(t *testing.T, what string, predicate func() bool) {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for !predicate() {
		if time.Now().After(until) {
			t.Fatal("receipt_fixture_timeout_" + what)
		}
		time.Sleep(time.Millisecond)
	}
}
func receiptDone(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("receipt_fixture_join_" + what)
	}
}
func receiptClient(t *testing.T, options AckBufferOptions) (*Client, *receiptPeer, func()) {
	t.Helper()
	p := newReceiptPeer(t)
	c, err := NewClient(context.Background(), ClientOptions{Identity: Identity{Oid: "00000000-0000-4000-8000-000000000001", InstallationKey: "synthetic-only"}, DestURL: "ws" + strings.TrimPrefix(p.server.URL, "http") + "/usp", Platform: "text", IsCompressed: true, BufferOptions: options, OnWarning: func(string) { p.warned.Add(1) }})
	if err != nil {
		p.close()
		t.Fatal("receipt_fixture_client")
	}
	var once sync.Once
	closeAll := func() { once.Do(func() { c.Close(); p.close() }) }
	t.Cleanup(closeAll)
	receiptWait(t, "flow", func() bool { return c.ab.GetCurrentCapacity() == 4 })
	return c, p, closeAll
}
func receiptAck(t *testing.T, c *Client, p *receiptPeer, seq uint64) {
	t.Helper()
	p.control(t, protocol.ControlMessage{Verb: protocol.ControlMessageACK, SeqNum: seq})
	receiptWait(t, "validated_ack", func() bool { c.ab.RLock(); defer c.ab.RUnlock(); return c.ab.firstSeqNum == seq+1 })
}
func TestAdmissionReceiptRealPeerProgress(t *testing.T) {
	var callbacks atomic.Int32
	c, p, closeAll := receiptClient(t, AckBufferOptions{OnAck: func() { callbacks.Add(1) }})
	message := &protocol.DataMessage{TextPayload: "synthetic-A"}
	a, err := c.ShipWithReceipt(message, time.Second)
	if err != nil {
		t.Fatal("receipt_fixture_admit_A")
	}
	p.nextFrame(t)
	if c.Ship(&protocol.DataMessage{TextPayload: "synthetic-untracked"}, time.Second) != nil {
		t.Fatal("receipt_fixture_untracked")
	}
	p.nextFrame(t)
	b, err := c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "synthetic-B"}, time.Second)
	if err != nil {
		t.Fatal("receipt_fixture_admit_B")
	}
	p.nextFrame(t)
	pending := c.ObserveReceipt(a) == (ReceiptObservation{Frame: ReceiptRetained, Lifetime: ReceiptLive}) && c.ObserveReceipt(b).Frame == ReceiptRetained
	foreign := &Client{}
	foreign.admissionReceiptOwner()
	invalid := c.ObserveReceipt(AdmissionReceipt{}).Frame == ReceiptInvalid && foreign.ObserveReceipt(a).Frame == ReceiptInvalid && c.ObserveReceipt(AdmissionReceipt{owner: a.owner, state: &receiptState{}}).Frame == ReceiptInvalid
	// This actual peer ACK reaches the existing pre-validation OnAck callback
	// but must fail the existing sequence validation and confirm no receipt.
	p.control(t, protocol.ControlMessage{Verb: protocol.ControlMessageACK, SeqNum: 999})
	receiptWait(t, "invalid_ack_callback", func() bool { return callbacks.Load() == 1 && p.warned.Load() == 1 })
	rejectedACK := c.ObserveReceipt(a).Frame == ReceiptRetained && c.ObserveReceipt(b).Frame == ReceiptRetained
	receiptAck(t, c, p, 1)
	partial := c.ObserveReceipt(a).Frame == ReceiptAcknowledged && c.ObserveReceipt(b).Frame == ReceiptRetained
	p.control(t, protocol.ControlMessage{Verb: protocol.ControlMessageFLOW, WindowSize: 1})
	receiptWait(t, "shrink", func() bool { return c.ab.GetCurrentCapacity() == 1 })
	refused, refusedErr := c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "refused"}, time.Millisecond)
	refusal := errors.Is(refusedErr, ErrorBufferFull) && refused == (AdmissionReceipt{})
	receiptAck(t, c, p, 2)
	again, againErr := c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "still-refused"}, time.Millisecond)
	full := errors.Is(againErr, ErrorBufferFull) && again == (AdmissionReceipt{})
	receiptAck(t, c, p, 3)
	later, laterErr := c.ShipWithReceipt(message, time.Second)
	if laterErr != nil {
		t.Fatal("receipt_fixture_reuse_pointer")
	}
	last := p.nextFrame(t)
	distinct := later != a && last.seq == 4 && c.ObserveReceipt(a).Frame == ReceiptAcknowledged && c.ObserveReceipt(later).Frame == ReceiptRetained
	receiptAck(t, c, p, last.seq)
	complete := c.ObserveReceipt(later).Frame == ReceiptAcknowledged && c.ObserveReceipt(b).Frame == ReceiptAcknowledged
	closeAll()
	historical := c.ObserveReceipt(a) == (ReceiptObservation{Frame: ReceiptAcknowledged, Lifetime: ReceiptClosed})
	if !pending || !invalid || !rejectedACK || !partial || !refusal || !full || !distinct || !complete || !historical || p.failed.Load() {
		t.Fatal("RECEIPT-ACK: exact validated cumulative ACK/admission identity or unchanged window contract violated")
	}
}
func TestAdmissionReceiptACKBeforeReturn(t *testing.T) {
	c, p, closeAll := receiptClient(t, AckBufferOptions{})
	// The real lazy owner-publication mutex holds only the return boundary;
	// admission and the peer's validated ACK remain the real production paths.
	c.receiptMu.Lock()
	var unlock sync.Once
	release := func() { unlock.Do(c.receiptMu.Unlock) }
	defer release()
	var token AdmissionReceipt
	var shipErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		token, shipErr = c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "early-ACK"}, time.Second)
	}()
	frame := p.nextFrame(t)
	receiptAck(t, c, p, frame.seq)
	blocked := false
	select {
	case <-done:
	default:
		blocked = true
	}
	release()
	receiptDone(t, "ship_return", done)
	observed := c.ObserveReceipt(token)
	closeAll()
	if !blocked || shipErr != nil || observed != (ReceiptObservation{Frame: ReceiptAcknowledged, Lifetime: ReceiptLive}) || p.failed.Load() {
		t.Fatal("RECEIPT-EARLY-ACK: ACK before successful return was lost or receipt published before admission")
	}
}
func TestAdmissionReceiptReconnectKeepsEntry(t *testing.T) {
	c, p, closeAll := receiptClient(t, AckBufferOptions{})
	token, err := c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "same-replay"}, time.Second)
	if err != nil {
		t.Fatal("receipt_fixture_reconnect_ship")
	}
	first := p.nextFrame(t)
	p.mu.Lock()
	p.current.Close()
	p.mu.Unlock()
	second := p.nextFrame(t)
	pending := first.seq == second.seq && second.generation > first.generation && c.ObserveReceipt(token) == (ReceiptObservation{Frame: ReceiptRetained, Lifetime: ReceiptLive})
	receiptAck(t, c, p, second.seq)
	ack := c.ObserveReceipt(token).Frame == ReceiptAcknowledged
	closeAll()
	if !pending || !ack || p.failed.Load() {
		t.Fatal("RECEIPT-REPLAY: reconnect replaced or falsely confirmed retained entry")
	}
}
func TestAdmissionReceiptCloseWaitsForRegisteredIO(t *testing.T) {
	c, p, closeAll := receiptClient(t, AckBufferOptions{})
	token, err := c.ShipWithReceipt(&protocol.DataMessage{TextPayload: "unconfirmed-tail"}, time.Second)
	if err != nil {
		t.Fatal("receipt_fixture_tail_ship")
	}
	p.nextFrame(t)
	release := make(chan struct{})
	var once sync.Once
	releaseWorker := func() { once.Do(func() { close(release) }) }
	defer releaseWorker()
	c.wg.Add(1)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); defer c.wg.Done(); <-release }()
	var tail []*protocol.DataMessage
	var closeErr error
	done := make(chan struct{})
	go func() { defer close(done); tail, closeErr = c.Close() }()
	receiptWait(t, "closing", c.isClosing.Load)
	select {
	case <-p.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("receipt_fixture_transport_exit")
	}
	during := c.ObserveReceipt(token)
	returned := false
	select {
	case <-done:
		returned = true
	default:
	}
	releaseWorker()
	receiptDone(t, "synthetic_registered_worker", workerDone)
	receiptDone(t, "actual_close", done)
	after := c.ObserveReceipt(token)
	closeAll()
	if returned || during.Lifetime != ReceiptClosing || during.Frame != ReceiptRetained || after != (ReceiptObservation{Frame: ReceiptClosedUnconfirmed, Lifetime: ReceiptClosed}) || len(tail) != 1 || tail[0].TextPayload != "unconfirmed-tail" || closeErr != nil || p.failed.Load() {
		t.Fatal("RECEIPT-CLOSE: closing intent replaced actual registered-IO join or manufactured confirmation")
	}
}

var receiptObservationSink ReceiptObservation

func TestAdmissionReceiptGenerationWrapAndAllocation(t *testing.T) {
	buffer, _ := NewAckBuffer(AckBufferOptions{})
	buffer.UpdateCapacity(4)
	c := &Client{ab: buffer}
	buffer.firstSeqNum, buffer.nextSeqNum = math.MaxUint64-1, math.MaxUint64-1
	tokens := make([]AdmissionReceipt, 3)
	for i := range tokens {
		var err error
		tokens[i], err = c.ShipWithReceipt(&protocol.DataMessage{}, 0)
		if err != nil {
			t.Fatal("receipt_fixture_wrap_admit")
		}
		buffer.GetNextToDeliver(0)
	}
	if buffer.Ack(math.MaxUint64) != nil {
		t.Fatal("receipt_fixture_wrap_ack")
	}
	wrap := c.ObserveReceipt(tokens[0]).Frame == ReceiptAcknowledged && c.ObserveReceipt(tokens[1]).Frame == ReceiptAcknowledged && c.ObserveReceipt(tokens[2]).Frame == ReceiptRetained
	if buffer.Ack(0) != nil {
		t.Fatal("receipt_fixture_wrap_final")
	}
	pending, err := c.ShipWithReceipt(&protocol.DataMessage{}, 0)
	if err != nil {
		t.Fatal("receipt_fixture_allocation_prefix")
	}
	allocations := testing.AllocsPerRun(1000, func() { receiptObservationSink = c.ObserveReceipt(pending) })
	buffer.Close()
	buffer.releaseReceiptTracking()
	// A refusal/sink path must never return a valid receipt or retain metadata.
	sink := &Client{options: ClientOptions{TestSinkMode: true}}
	sinkToken, sinkErr := sink.ShipWithReceipt(&protocol.DataMessage{}, 0)
	refused, refusedErr := c.ShipWithReceipt(&protocol.DataMessage{}, 0)
	// Compare steady-state opt-in overhead against the unchanged untracked
	// admission path with the same already-delivered/ACKed single-entry workload.
	measured, _ := NewAckBuffer(AckBufferOptions{})
	plain := &Client{ab: measured}
	sample := &protocol.DataMessage{}
	plainAlloc := testing.AllocsPerRun(100, func() {
		if plain.Ship(sample, 0) != nil {
			panic("allocation fixture")
		}
		measured.GetNextToDeliver(0)
		if measured.Ack(sample.SeqNum) != nil {
			panic("allocation fixture")
		}
	})
	trackedAlloc := testing.AllocsPerRun(100, func() {
		if _, e := plain.ShipWithReceipt(sample, 0); e != nil {
			panic("allocation fixture")
		}
		measured.GetNextToDeliver(0)
		if measured.Ack(sample.SeqNum) != nil {
			panic("allocation fixture")
		}
	})
	measured.Close()
	measured.releaseReceiptTracking()
	t.Logf("RECEIPT-ALLOCS untracked=%.0f tracked=%.0f extra=%.0f", plainAlloc, trackedAlloc, trackedAlloc-plainAlloc)
	t.Logf("RECEIPT-SIZES state=%d owner=%d link=%d token=%d observation=%d ObserveAllocs=%.0f", unsafe.Sizeof(receiptState{}), unsafe.Sizeof(receiptOwner{}), unsafe.Sizeof(trackedAdmission{}), unsafe.Sizeof(AdmissionReceipt{}), unsafe.Sizeof(ReceiptObservation{}), allocations)
	if !wrap || allocations != 0 || trackedAlloc-plainAlloc > 2 || sinkErr != ErrorReceiptUnavailable || sinkToken != (AdmissionReceipt{}) || refusedErr != ErrorClosed || refused != (AdmissionReceipt{}) || buffer.receiptHead != nil {
		t.Fatal("RECEIPT-BOUND: entry wrap, zero-allocation observer or invalid refusal/sink contract violated")
	}
}
