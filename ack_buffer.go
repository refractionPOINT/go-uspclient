package uspclient

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/refractionPOINT/go-uspclient/protocol"
)

const (
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

	// wake is closed (and nilled) whenever something a blocked Add may be
	// waiting for changes: room freed by an ack, capacity change, Close.
	// It is created lazily by the first Add that has to block.
	wake chan struct{}
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
	b.releaseAllLocked()
	b.signalLocked()
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
// It returns false on timeout or if the buffer is (or becomes) closed.
//
// A buffer that holds nothing always admits one message, even one larger than
// the byte caps, so a single large message can never deadlock. The shared
// budget can therefore be overshot by at most one message per client.
func (b *AckBuffer) Add(e *protocol.DataMessage, timeout time.Duration) bool {
	if e == nil {
		return false
	}

	var size uint64
	if b.accounting {
		size = estimateMessageSize(e)
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
			return false
		}
		ok, poolWake := b.tryAddLocked(e, size)
		if ok {
			b.Unlock()
			return true
		}
		if b.wake == nil {
			b.wake = make(chan struct{})
		}
		localWake := b.wake
		b.Unlock()

		if !deadlineC.IsZero() {
			remaining := time.Until(deadlineC)
			if remaining <= 0 {
				return false
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
			return false
		}
	}
}

// tryAddLocked admits e if there is room. When it fails because the shared
// budget is full it also returns a channel that is closed on the budget's next
// release; the channel was obtained atomically with the failed reservation so
// no release can be missed. Must hold the lock.
func (b *AckBuffer) tryAddLocked(e *protocol.DataMessage, size uint64) (bool, <-chan struct{}) {
	if !b.isAvailable.IsSet() {
		return false, nil
	}
	isEmpty := b.nextIndexFree == 0
	if b.maxBytes != 0 && !isEmpty && b.bytes+size > b.maxBytes {
		return false, nil
	}
	if b.shared != nil {
		if ok, wake := b.shared.reserve(size, isEmpty); !ok {
			return false, wake
		}
	}

	e.SeqNum = b.nextSeqNum
	b.nextSeqNum++
	if e.SeqNum%b.ackEvery == 0 {
		e.AckRequested = true
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
