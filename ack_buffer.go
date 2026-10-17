package uspclient

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/refractionPOINT/go-uspclient/protocol"
)

const (
	// pressureStepDivisor: under byte pressure, ask for an ack every
	// 1/pressureStepDivisor of the limit admitted. See tryAddLocked.
	pressureStepDivisor = 16

	initialCapacity      = 1
	ackPercentOfCapacity = 0.5
)

type AckBufferOptions struct {
	OnBackPressure func() `json:"-" yaml:"-" msgpack:"-"`
	OnAck          func() `json:"-" yaml:"-" msgpack:"-"`

	// MaxBytes caps the estimated in-memory size of the messages this buffer
	// holds while they are waiting to be acknowledged. 0 means no per-client
	// byte cap. Ship() blocks (like it does when the message-count window is
	// full) until acks free enough room. See estimateMessageSize for how
	// sizes are computed.
	MaxBytes uint64 `json:"max_bytes,omitempty" yaml:"max_bytes,omitempty" msgpack:"max_bytes,omitempty"`

	// SharedBudget is an optional process-wide byte budget that many clients
	// draw from (see NewByteBudget). nil means no shared budget. It is never
	// serialized: it only makes sense inside one process.
	SharedBudget *ByteBudget `json:"-" yaml:"-" msgpack:"-"`
}

type AckBuffer struct {
	sync.RWMutex

	isClosed    bool
	isRunning   bool
	isAvailable *Event

	buff          []*protocol.DataMessage
	nextIndexFree uint64
	firstSeqNum   uint64
	nextSeqNum    uint64
	ackEvery      uint64

	nextIndexToDeliver uint64
	isReadyToDeliver   *Event
	isEmpty            *Event

	currentCapacity uint64

	onBackPressure func()
	onAck          func()

	// Byte accounting. Only active when maxBytes != 0 or shared != nil, so
	// that buffers without a byte budget pay nothing for it.
	accounting bool
	maxBytes   uint64
	shared     *ByteBudget
	// sizes[i] is the estimated size of buff[i]; len(sizes) == nextIndexFree.
	sizes []uint64
	// bytes is what this buffer currently holds and has charged to the shared
	// budget. It is zeroed (and returned to the budget) by Close.
	bytes uint64

	// lastFlaggedSeq is the sequence number of the most recent message that
	// carries AckRequested (0 = none yet). Acks are cumulative, so a flagged
	// message is still held iff lastFlaggedSeq >= firstSeqNum. See tryAddLocked.
	lastFlaggedSeq uint64
	// bytesSinceFlag is the estimated size admitted since the last message
	// that carried AckRequested.
	bytesSinceFlag uint64

	// wake is closed (and nilled) whenever something a blocked Add may be
	// waiting for changes: room freed by an ack, capacity change, Close.
	// It is created lazily by the first Add that has to block.
	wake chan struct{}

	// Only the opt-in receipt API allocates this map. Admission and receipt
	// registration share the lock, so even an immediate ACK cannot be missed.
	receipts map[uint64]*Receipt
}

func NewAckBuffer(o AckBufferOptions) (*AckBuffer, error) {
	b := &AckBuffer{
		buff:             make([]*protocol.DataMessage, initialCapacity),
		ackEvery:         initialCapacity,
		isRunning:        true,
		isAvailable:      NewEvent(),
		firstSeqNum:      1,
		nextSeqNum:       1,
		isReadyToDeliver: NewEvent(),
		isEmpty:          NewEvent(),
		currentCapacity:  initialCapacity,
		onBackPressure:   o.OnBackPressure,
		onAck:            o.OnAck,
		maxBytes:         o.MaxBytes,
		shared:           o.SharedBudget,
		accounting:       o.MaxBytes != 0 || o.SharedBudget != nil,
	}
	b.isAvailable.Set()
	b.isEmpty.Set()

	return b, nil
}

func (b *AckBuffer) GetEmptyEvent() *Event {
	return b.isEmpty
}

// Close stops the buffer from accepting new messages, wakes any Add call that
// is blocked waiting for room (they return false) and returns the bytes it
// holds to the shared budget, if any. Messages already in the buffer stay
// available to GetUnAcked / GetNextToDeliver so they can still be drained or
// handed back to the caller. It is safe to call more than once.
func (b *AckBuffer) Close() {
	b.Lock()
	defer b.Unlock()
	b.isRunning = false
	b.isClosed = true
	for seq, receipt := range b.receipts {
		receipt.finish(ErrorClosed)
		delete(b.receipts, seq)
	}
	b.receipts = nil
	b.releaseAllLocked()
	b.signalLocked()
}

// drain seals admission without returning bytes to the shared pool: retained
// messages still consume memory until an ACK or Close. The wake channel is
// sampled under the same lock as the pending count, so no final ACK is missed.
func (b *AckBuffer) drain(ctx context.Context) error {
	b.Lock()
	b.isRunning = false
	b.signalLocked()
	for {
		if b.nextIndexFree == 0 {
			b.Unlock()
			return nil
		}
		err := ctx.Err()
		if b.isClosed {
			err = ErrorClosed
		}
		if err != nil {
			e := &DrainError{Unacked: int(b.nextIndexFree), FirstSeq: b.buff[0].SeqNum,
				LastSeq: b.buff[b.nextIndexFree-1].SeqNum, Err: err}
			b.Unlock()
			return e
		}
		if b.wake == nil {
			b.wake = make(chan struct{})
		}
		wake := b.wake
		b.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
		}
		b.Lock()
	}
}

// signalLocked wakes every Add blocked on this buffer. Must hold the lock.
func (b *AckBuffer) signalLocked() {
	if b.wake != nil {
		close(b.wake)
		b.wake = nil
	}
}

// releaseAllLocked returns everything this buffer has charged to the shared
// budget. Must hold the lock.
func (b *AckBuffer) releaseAllLocked() {
	if b.bytes != 0 {
		b.shared.release(b.bytes)
		b.bytes = 0
	}
}

// BufferedBytes returns the estimated size of the buffered, unacked messages.
// It is only tracked when MaxBytes or SharedBudget is configured, otherwise it
// is always 0. After Close it is 0.
func (b *AckBuffer) BufferedBytes() uint64 {
	b.RLock()
	defer b.RUnlock()
	return b.bytes
}

// Add queues a message, blocking while the buffer has no room: the
// message-count window is full, or the per-client / shared byte budget would
// be exceeded. timeout == 0 waits until there is room or the buffer is closed.
// It returns false on timeout or if the buffer is (or becomes) closed; use the
// client's Ship to tell the two apart.
//
// A buffer that holds no message with an ack request pending (in particular an
// empty one) admits one message even if it exceeds the byte caps, and flags it
// with AckRequested, so a single large message can never deadlock and a client
// never waits on bytes without an ack already requested. The shared budget can
// therefore be overshot by at most one message per client.
func (b *AckBuffer) Add(e *protocol.DataMessage, timeout time.Duration) bool {
	return b.add(e, timeout) == nil
}

// add is Add with the reason for a failure: ErrorClosed when the buffer is (or
// becomes) closed, ErrorBufferFull when the timeout expired (or e is nil).
func (b *AckBuffer) add(e *protocol.DataMessage, timeout time.Duration) error {
	return b.addWithReceipt(e, timeout, nil)
}

func (b *AckBuffer) addWithReceipt(e *protocol.DataMessage, timeout time.Duration, receipt *Receipt) error {
	if e == nil {
		return ErrorBufferFull
	}

	var size uint64
	if b.accounting {
		size = estimateMessageSize(e)
		if receipt != nil {
			size += receiptOverheadBytes
		}
	}

	var (
		timer     *time.Timer
		timerC    <-chan time.Time
		deadlineC time.Time
	)
	if timeout != 0 {
		deadlineC = time.Now().Add(timeout)
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()
	}

	for {
		b.Lock()
		if !b.isRunning {
			b.Unlock()
			return ErrorClosed
		}
		ok, poolWake := b.tryAddLocked(e, size)
		if ok {
			if receipt != nil {
				if b.receipts == nil {
					b.receipts = make(map[uint64]*Receipt)
				}
				b.receipts[e.SeqNum] = receipt
			}
			b.Unlock()
			return nil
		}
		if b.wake == nil {
			b.wake = make(chan struct{})
		}
		localWake := b.wake
		b.Unlock()

		if !deadlineC.IsZero() {
			remaining := time.Until(deadlineC)
			if remaining <= 0 {
				return b.timeoutErr()
			}
			if b.onBackPressure != nil {
				b.onBackPressure()
			}
			if timer == nil {
				timer = time.NewTimer(remaining)
				timerC = timer.C
			}
		}

		select {
		case <-localWake:
		case <-poolWake: // nil (blocks forever) when not waiting on the pool
		case <-timerC:
			return b.timeoutErr()
		}
	}
}

// timeoutErr is the error for an Add that ran out of time: the buffer being
// closed at that moment takes precedence over the timeout.
func (b *AckBuffer) timeoutErr() error {
	b.RLock()
	defer b.RUnlock()
	if !b.isRunning {
		return ErrorClosed
	}
	return ErrorBufferFull
}

// tryAddLocked admits e if there is room. When it fails because the shared
// budget is full it also returns a channel that is closed on the budget's next
// release; the channel was obtained atomically with the failed reservation so
// no release can be missed. Must hold the lock.
//
// Byte limits and ack requests. The backend only acknowledges messages that
// carry AckRequested (plus a slow periodic ack), and the count-based rule sets
// it only every ackEvery messages, which can be tens of thousands. A client
// that blocks on bytes long before that would wait for the slow periodic ack.
// Two rules prevent it:
//
//   - Guarantee: a client never blocks on bytes unless it holds an unacked
//     message that carries AckRequested. If it holds none and the bytes would
//     not fit, the message is admitted anyway (the same one-message overshoot
//     as an empty buffer) and flagged, so the ack that unblocks the client is
//     already on its way. The flag is only forced when the limit would
//     actually have blocked, so there is no extra ack traffic otherwise.
//   - Throughput: once a limit is at least half used, a message is flagged
//     whenever 1/16th of that limit has been admitted since the last flag. A
//     blocked client then has all but the last 1/16th of the limit covered by
//     requests, so one ack releases nearly everything it holds, at a cost of
//     about 16 requests per limit's worth of data (not one per message).
//     Below the half-way point (steady state, plenty of room) no extra acks
//     are asked.
func (b *AckBuffer) tryAddLocked(e *protocol.DataMessage, size uint64) (bool, <-chan struct{}) {
	if !b.isAvailable.IsSet() {
		return false, nil
	}
	forced := false
	var pressureStep uint64
	if b.accounting {
		heldFlagged := b.lastFlaggedSeq != 0 && b.lastFlaggedSeq >= b.firstSeqNum
		if b.maxBytes != 0 {
			if b.bytes+size > b.maxBytes {
				if heldFlagged {
					return false, nil
				}
				forced = true
			}
			if 2*(b.bytes+size) >= b.maxBytes {
				pressureStep = b.maxBytes / pressureStepDivisor
			}
		}
		if b.shared != nil {
			ok, overshot, used, wake := b.shared.reserve(size, !heldFlagged)
			if !ok {
				return false, wake
			}
			forced = forced || overshot
			if total := b.shared.Total(); total != 0 && 2*used >= total {
				if step := total / pressureStepDivisor; pressureStep == 0 || step < pressureStep {
					pressureStep = step
				}
			}
		}
	}

	e.SeqNum = b.nextSeqNum
	b.nextSeqNum++
	if e.SeqNum%b.ackEvery == 0 || forced || (pressureStep != 0 && b.bytesSinceFlag+size >= pressureStep) {
		e.AckRequested = true
	}
	if e.AckRequested {
		b.lastFlaggedSeq = e.SeqNum
		b.bytesSinceFlag = 0
	} else if b.accounting {
		b.bytesSinceFlag += size
	}
	b.buff[b.nextIndexFree] = e
	b.nextIndexFree++
	if b.accounting {
		b.sizes = append(b.sizes, size)
		b.bytes += size
	}
	if b.nextIndexFree >= b.currentCapacity {
		b.isAvailable.Clear()
	}
	b.isReadyToDeliver.Set()
	b.isEmpty.Clear()
	return true, nil
}

func (b *AckBuffer) Ack(seq uint64) error {
	if b.onAck != nil {
		b.onAck()
	}
	b.Lock()
	defer b.Unlock()
	if seq < b.firstSeqNum && uint64(len(b.buff))-(math.MaxUint64-b.firstSeqNum) <= seq {
		return fmt.Errorf("unexpected acked sequence number: %d", seq)
	}
	if seq >= b.nextSeqNum && seq != math.MaxUint64 && b.nextSeqNum != 0 {
		return fmt.Errorf("unexpected acked sequence number: %d", seq)
	}
	indexAcked := seq - b.firstSeqNum

	// Check if the message exists in the buffer
	if indexAcked >= uint64(len(b.buff)) || indexAcked >= b.nextIndexFree {
		return fmt.Errorf("acked message (seq: %d) is not in buffer", seq)
	}

	// Check if the message has been delivered, but with special handling for edge cases
	if indexAcked >= b.nextIndexToDeliver {
		// Special case: if nextIndexToDeliver is 0 (likely due to ResetDelivery)
		// and we're trying to ACK the first message in the buffer, allow it
		if b.nextIndexToDeliver == 0 && indexAcked == 0 {
			// This is the ResetDelivery scenario - allow ACKing the first message
		} else {
			nextSeq := uint64(0)
			if b.nextIndexToDeliver < uint64(len(b.buff)) && b.buff[b.nextIndexToDeliver] != nil {
				nextSeq = b.buff[b.nextIndexToDeliver].SeqNum
			}
			return fmt.Errorf("acked message (seq: %d, index: %d) has not yet been delivered (next_seq: %d, next_index: %d)", seq, indexAcked, nextSeq, b.nextIndexToDeliver)
		}
	}

	// A validated cumulative ACK confirms each covered receipt. Complete only
	// after every ACK validation above has succeeded, never on an invalid ACK.
	if len(b.receipts) != 0 {
		for _, message := range b.buff[:indexAcked+1] {
			if receipt := b.receipts[message.SeqNum]; receipt != nil {
				receipt.finish(nil)
				delete(b.receipts, message.SeqNum)
			}
		}
	}
	b.firstSeqNum = seq + 1
	if b.accounting {
		var freed uint64
		for _, sz := range b.sizes[:indexAcked+1] {
			freed += sz
		}
		b.sizes = b.sizes[:copy(b.sizes, b.sizes[indexAcked+1:])]
		// After Close the bytes were already returned: never release twice.
		if freed > b.bytes {
			freed = b.bytes
		}
		b.bytes -= freed
		b.shared.release(freed)
	}
	for i := indexAcked + 1; i < uint64(len(b.buff)); i++ {
		b.buff[i-indexAcked-1] = b.buff[i]
	}
	b.nextIndexFree = b.nextIndexFree - indexAcked - 1
	for i := b.nextIndexFree; i < uint64(len(b.buff)); i++ {
		b.buff[i] = nil
	}
	b.isAvailable.Set()
	b.signalLocked()

	// Adjust nextIndexToDeliver to account for removed messages
	if b.nextIndexToDeliver > indexAcked+1 {
		b.nextIndexToDeliver -= (indexAcked + 1)
	} else {
		b.nextIndexToDeliver = 0
	}

	if b.nextIndexToDeliver >= b.nextIndexFree {
		b.isReadyToDeliver.Clear()
		b.isEmpty.Set()
	}
	return nil
}

func (b *AckBuffer) GetUnAcked() ([]*protocol.DataMessage, error) {
	b.RLock()
	defer b.RUnlock()
	out := make([]*protocol.DataMessage, b.nextIndexFree)
	copy(out, b.buff[:b.nextIndexFree])
	return out, nil
}

func (b *AckBuffer) GetNextToDeliver(timeout time.Duration) *protocol.DataMessage {
	if !b.isReadyToDeliver.WaitFor(timeout) {
		return nil
	}
	b.Lock()
	defer b.Unlock()
	if !b.isReadyToDeliver.IsSet() {
		return nil
	}
	n := b.buff[b.nextIndexToDeliver]
	if !b.isRunning && b.nextIndexToDeliver+1 == b.nextIndexFree {
		// Do not mutate the retained message while a sender may encode it.
		// Cumulative ACKs need one extra request, on the final queued frame.
		// Already transmitted tails are confirmed by the proxy's periodic ACK.
		copy := *n
		copy.AckRequested = true
		n = &copy
	}
	b.nextIndexToDeliver++
	if b.nextIndexToDeliver >= b.nextIndexFree {
		b.isReadyToDeliver.Clear()
		b.isEmpty.Set()
	}
	return n
}

func (b *AckBuffer) ResetDelivery() {
	b.Lock()
	defer b.Unlock()
	b.nextIndexToDeliver = 0
	if b.nextIndexToDeliver >= b.nextIndexFree {
		b.isReadyToDeliver.Clear()
		b.isEmpty.Set()
	} else {
		b.isReadyToDeliver.Set()
		b.isEmpty.Clear()
	}
}

func (b *AckBuffer) GetCurrentCapacity() uint64 {
	b.Lock()
	defer b.Unlock()
	return b.currentCapacity
}

func (b *AckBuffer) UpdateCapacity(newCapacity uint64) {
	b.Lock()
	defer b.Unlock()

	if newCapacity == b.currentCapacity {
		return
	}

	// We only ever grow the capacity.
	if uint64(len(b.buff)) < newCapacity {
		newBuff := make([]*protocol.DataMessage, newCapacity)
		copy(newBuff, b.buff)
		b.buff = newBuff
		b.isAvailable.Set()
	} else if b.currentCapacity < newCapacity {
		if b.nextIndexFree == b.currentCapacity {
			b.isAvailable.Set()
		}
	} else if b.currentCapacity > newCapacity {
		if b.nextIndexFree >= newCapacity {
			b.isAvailable.Clear()
		}
	}
	b.currentCapacity = newCapacity
	b.signalLocked()
	b.ackEvery = uint64(float64(newCapacity) * ackPercentOfCapacity)
	if b.ackEvery == 0 {
		b.ackEvery = 1
	}
}
