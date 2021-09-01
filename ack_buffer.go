package uspclient

import (
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	defaultCapacity      = 1000
	ackPercentOfCapacity = 0.9
)

type AckBufferOptions struct {
	BufferCapacity uint64
}

type AckBuffer struct {
	sync.RWMutex

	isAvailable *Event

	buff        []*UspDataMessage
	indexNext   uint64
	firstSeqNum uint64
	nextSeqNum  uint64
	ackEvery    uint64
}

func NewAckBuffer(o AckBufferOptions) (*AckBuffer, error) {
	if o.BufferCapacity == 0 {
		o.BufferCapacity = defaultCapacity
	}
	b := &AckBuffer{
		buff:        make([]*UspDataMessage, o.BufferCapacity),
		ackEvery:    uint64(float64(o.BufferCapacity) * ackPercentOfCapacity),
		isAvailable: NewEvent(),
		firstSeqNum: 1,
	}
	b.isAvailable.Set()

	return b, nil
}

func (b *AckBuffer) Add(e *UspDataMessage, timeout time.Duration) bool {
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
			if !b.isAvailable.WaitFor(deadline.Sub(now)) {
				break
			}
		}

		b.Lock()
		if !b.isAvailable.IsSet() {
			b.Unlock()
			if !deadline.IsZero() {
				continue
			}
			break
		}

		e.SeqNum = b.nextSeqNum
		b.nextSeqNum++
		if e.SeqNum%b.ackEvery == 0 {
			e.AckRequested = true
		}
		b.buff[b.indexNext] = e
		b.indexNext++
		if b.indexNext >= uint64(len(b.buff)) {
			b.isAvailable.Clear()
		}
		hasBeenAdded = true
		b.Unlock()
	}

	return hasBeenAdded
}

func (b *AckBuffer) Ack(seq uint64) error {
	b.Lock()
	defer b.Unlock()
	if seq < b.firstSeqNum {
		return fmt.Errorf("unexpected acked sequence number: %d", seq)
	}
	if seq >= b.nextSeqNum && b.nextSeqNum >= math.MaxUint64-seq {
		return fmt.Errorf("unexpected acked sequence number: %d", seq)
	}
	indexAcked := seq - b.firstSeqNum
	b.firstSeqNum = seq + 1
	for i := indexAcked + 1; i < uint64(len(b.buff)); i++ {
		b.buff[i-indexAcked-1] = b.buff[i]
	}
	b.indexNext = b.indexNext - indexAcked - 1
	for i := b.indexNext; i < uint64(len(b.buff)); i++ {
		b.buff[i] = nil
	}
	b.isAvailable.Set()
	return nil
}

func (b *AckBuffer) GetUnAcked() ([]*UspDataMessage, error) {
	b.RLock()
	defer b.RUnlock()
	out := make([]*UspDataMessage, b.indexNext)
	copy(out, b.buff[:b.indexNext])
	return out, nil
}
