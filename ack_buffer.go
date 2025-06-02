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
	OnBackPressure func() `json:"-" yaml:"-"`
	OnAck          func() `json:"-" yaml:"-"`
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
	}
	b.isAvailable.Set()
	b.isEmpty.Set()

	return b, nil
}

func (b *AckBuffer) GetEmptyEvent() *Event {
	return b.isEmpty
}

func (b *AckBuffer) Close() {
	b.Lock()
	defer b.Unlock()
	b.isRunning = false
}

func (b *AckBuffer) Add(e *protocol.DataMessage, timeout time.Duration) bool {
	if e == nil {
		return false
	}

	b.RLock()
	isRunning := b.isRunning
	b.RUnlock()
	if !isRunning {
		return false
	}

	hasBeenAdded := false
	var deadline time.Time
	if timeout != 0 {
		deadline = time.Now().Add(timeout)
	}
	for !hasBeenAdded {
		if !deadline.IsZero() {
			now := time.Now()
			if now.After(deadline) {
				break
			}
			if b.onBackPressure != nil && !b.isAvailable.IsSet() {
				b.onBackPressure()
			}
			if !b.isAvailable.WaitFor(deadline.Sub(now)) {
				break
			}
		} else {
			b.isAvailable.WaitFor(500 * time.Millisecond)
		}

		b.Lock()
		if !b.isAvailable.IsSet() {
			isRunning := b.isRunning
			b.Unlock()
			if !isRunning {
				break
			}
			if !deadline.IsZero() && time.Now().After(deadline) {
				break
			}
			continue
		}

		e.SeqNum = b.nextSeqNum
		b.nextSeqNum++
		if e.SeqNum%b.ackEvery == 0 {
			e.AckRequested = true
		}
		b.buff[b.nextIndexFree] = e
		b.nextIndexFree++
		if b.nextIndexFree >= b.currentCapacity {
			b.isAvailable.Clear()
		}
		hasBeenAdded = true
		b.isReadyToDeliver.Set()
		b.isEmpty.Clear()
		b.Unlock()
	}

	return hasBeenAdded
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
	if indexAcked >= b.nextIndexToDeliver {
		nextSeq := uint64(0)
		if b.nextIndexToDeliver < uint64(len(b.buff)) && b.buff[b.nextIndexToDeliver] != nil {
			nextSeq = b.buff[b.nextIndexToDeliver].SeqNum
		}
		return fmt.Errorf("acked message (seq: %d) has not yet been delivered (next: %d)", seq, nextSeq)
	}
	b.firstSeqNum = seq + 1
	for i := indexAcked + 1; i < uint64(len(b.buff)); i++ {
		b.buff[i-indexAcked-1] = b.buff[i]
	}
	b.nextIndexFree = b.nextIndexFree - indexAcked - 1
	for i := b.nextIndexFree; i < uint64(len(b.buff)); i++ {
		b.buff[i] = nil
	}
	b.isAvailable.Set()
	b.nextIndexToDeliver -= (indexAcked + 1)
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
	b.ackEvery = uint64(float64(newCapacity) * ackPercentOfCapacity)
	if b.ackEvery == 0 {
		b.ackEvery = 1
	}
}
