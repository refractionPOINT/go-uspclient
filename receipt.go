package uspclient

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/refractionPOINT/go-uspclient/protocol"
)

// AdmissionReceipt identifies one successful opt-in buffer admission. Its fields
// are opaque: sequence numbers and payload pointer reuse cannot construct one.
// Keeping a receipt retains only its small state/owner, never Client or payload.
type AdmissionReceipt struct {
	owner *receiptOwner
	state *receiptState
}

// ReceiptFrameState is local protocol knowledge, not downstream processing or
// durable delivery. Only the existing validated ACK-removal path acknowledges.
type ReceiptFrameState uint32

const (
	ReceiptInvalid ReceiptFrameState = iota
	ReceiptRetained
	ReceiptAcknowledged
	ReceiptClosedUnconfirmed
)

// ReceiptLifetime distinguishes closing intent from actual registered-I/O join.
type ReceiptLifetime uint32

const (
	ReceiptUnknown ReceiptLifetime = iota
	ReceiptLive
	ReceiptClosing
	ReceiptClosed
)

// ReceiptObservation contains no payload, sequence, client or routing data.
// A conservative confirmation consumer requires BOTH ReceiptLive and
// ReceiptAcknowledged. Acknowledged remains historical fact after Close.
type ReceiptObservation struct {
	Frame    ReceiptFrameState
	Lifetime ReceiptLifetime
}

type receiptOwner struct{ lifetime atomic.Uint32 }
type receiptState struct{ state atomic.Uint32 }

// These links exist only while tracked entries are retained. Cells are separate
// allocations so a token never retains the linked metadata, Client or payload.
type trackedAdmission struct {
	seq   uint64
	state *receiptState
	next  *trackedAdmission
}

// ErrorReceiptUnavailable means an opt-in receipt cannot be produced by a test
// sink which deliberately accepts without a real buffer admission. Ship's
// existing test-sink behavior remains unchanged.
var ErrorReceiptUnavailable = errors.New("admission receipt unavailable in test sink mode")

// ShipWithReceipt shares Ship's actual admission, sequence and budget path.
// On error it returns an invalid receipt. A peer can acknowledge the entry
// before this method returns; that acknowledgement is preserved in its cell.
func (c *Client) ShipWithReceipt(message *protocol.DataMessage, timeout time.Duration) (AdmissionReceipt, error) {
	if c.options.TestSinkMode {
		return AdmissionReceipt{}, ErrorReceiptUnavailable
	}
	if c.ab == nil {
		return AdmissionReceipt{}, ErrorClosed
	}
	state := &receiptState{}
	if err := c.ab.addWithReceipt(message, timeout, state); err != nil {
		return AdmissionReceipt{}, err
	}
	// Bind the opaque owner only after real admission. The state was already
	// attached before publication, including any ACK racing this return.
	return AdmissionReceipt{owner: c.admissionReceiptOwner(), state: state}, nil
}
func (c *Client) admissionReceiptOwner() *receiptOwner {
	c.receiptMu.Lock()
	defer c.receiptMu.Unlock()
	owner := c.receiptOwner.Load()
	if owner == nil {
		owner = &receiptOwner{}
		lifetime := c.receiptLifetime
		if c.isClosing.Load() && lifetime < ReceiptClosing {
			lifetime = ReceiptClosing
		}
		if lifetime == ReceiptUnknown {
			lifetime = ReceiptLive
		}
		owner.lifetime.Store(uint32(lifetime))
		c.receiptOwner.Store(owner)
	}
	return owner
}
func (c *Client) setReceiptLifetime(lifetime ReceiptLifetime) {
	c.receiptMu.Lock()
	defer c.receiptMu.Unlock()
	if lifetime > c.receiptLifetime {
		c.receiptLifetime = lifetime
	}
	if owner := c.receiptOwner.Load(); owner != nil {
		owner.lifetime.Store(uint32(c.receiptLifetime))
	}
}

// ObserveReceipt is O(1), lock-free and allocation-free regardless of retained
// backlog, FLOW window or sequence distance. Foreign/zero/never-admitted tokens
// are invalid. Missing state and closing/closed lifetime never imply an ACK.
func (c *Client) ObserveReceipt(receipt AdmissionReceipt) ReceiptObservation {
	if c == nil || receipt.owner == nil || receipt.state == nil || c.receiptOwner.Load() != receipt.owner {
		return ReceiptObservation{}
	}
	state := ReceiptFrameState(receipt.state.state.Load())
	if state != ReceiptRetained && state != ReceiptAcknowledged {
		return ReceiptObservation{}
	}
	lifetime := ReceiptLifetime(receipt.owner.lifetime.Load())
	// Frame first, lifetime second prevents a closing client appearing live
	// with an ACK learned only during Close. Once joined, a final reread is
	// stable: no registered receiver can publish another ACK.
	if lifetime == ReceiptClosed {
		state = ReceiptFrameState(receipt.state.state.Load())
	}
	if c.isClosing.Load() && lifetime < ReceiptClosing {
		lifetime = ReceiptClosing
	}
	if lifetime == ReceiptClosed && state == ReceiptRetained {
		state = ReceiptClosedUnconfirmed
	}
	return ReceiptObservation{Frame: state, Lifetime: lifetime}
}

// Called only after Ack has validated and removed the cumulative prefix. The
// subtraction is the same unsigned wrap-safe entry distance as Ack's index.
func (b *AckBuffer) ackReceiptsLocked(first, index uint64) {
	for entry := b.receiptHead; entry != nil && entry.seq-first <= index; entry = b.receiptHead {
		b.receiptHead = entry.next
		entry.next = nil
		entry.state.state.Store(uint32(ReceiptAcknowledged))
	}
	if b.receiptHead == nil {
		b.receiptTail = nil
	}
}
func (b *AckBuffer) releaseReceiptTracking() {
	b.Lock()
	b.receiptHead, b.receiptTail = nil, nil
	b.Unlock()
}
