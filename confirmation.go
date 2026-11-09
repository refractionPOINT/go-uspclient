package uspclient

import (
	"context"
	"errors"
)

// ErrorInvalidReceipt means a confirmation barrier contains a zero receipt or
// one issued by another client lifetime.
var ErrorInvalidReceipt = errors.New("invalid admission receipt")

// ErrorReceiptRetired means a required frame exhausted its transmission limit.
// Retirement is never remote confirmation, even if a later frame is ACKed.
var ErrorReceiptRetired = errors.New("admission receipt retired without confirmation")

// WaitForReceipts is a nonterminal confirmation barrier for a source interval.
// Pass every admission receipt required by that interval, including all fragments.
// A failed admission must prevent committing the interval, even if its other
// admissions are confirmed. An empty set has no delivery dependencies.
//
// Only validated cumulative ACKs confirm receipts; OnAck is not confirmation.
// ACK semantics depend on the peer: they do not promise durable downstream
// storage or successful processing of every original event. Reconnect preserves
// receipt identity and confirmation. Foreign and zero receipts are rejected.
//
// Waiting does not seal admission or wait for unrelated frames. The caller owns
// cancellation and any deadline; cancellation leaves receipts usable for retry.
// Closing the client cancels the barrier, including previously confirmed sets.
// Keep receipt sets bounded to the source interval; receipts retain no payloads.
func (c *Client) WaitForReceipts(ctx context.Context, receipts ...AdmissionReceipt) error {
	if c == nil {
		return ErrorClosed
	}
	if c.options.TestSinkMode {
		return ErrorReceiptUnavailable
	}
	if c.ab == nil {
		return ErrorClosed
	}
	owner := c.receiptOwner.Load()
	for _, receipt := range receipts {
		if receipt.owner == nil || receipt.state == nil || receipt.owner != owner {
			return ErrorInvalidReceipt
		}
	}
	b := c.ab
	b.Lock()
	defer b.Unlock()
	for {
		if b.isClosed || c.isClosing.Load() {
			return ErrorClosed
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		complete := true
		for _, receipt := range receipts {
			switch ReceiptFrameState(receipt.state.state.Load()) {
			case ReceiptAcknowledged:
			case ReceiptRetained:
				complete = false
			case ReceiptRetired:
				return ErrorReceiptRetired
			default:
				return ErrorInvalidReceipt
			}
		}
		if complete {
			return nil
		}
		// ACK validation, receipt publication and wake sampling share the buffer
		// lock, so an ACK before registration or during sleep cannot be missed.
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
