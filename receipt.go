package uspclient

import "context"

// Include the receipt, its completion channel and its map entry in a tracked
// message's byte estimate. Source callback stacks and raw source payloads are
// owned by the source library and are outside this SDK budget.
const receiptOverheadBytes uint64 = 256

// Receipt tracks one admitted message until the proxy acknowledges it or the
// client closes. Confirmation has the semantics provided by the connected
// proxy; older proxies may acknowledge before forwarding to their downstream.
// A Receipt does not retain the message payload.
type Receipt struct {
	done chan struct{}
	err  error // Written before done closes; only read after done is closed.
}

// Wait waits for proxy confirmation. Canceling ctx cancels only this wait:
// the client still owns and may retransmit the admitted message until Close.
// ErrorClosed means the client closed without confirmation. A successful
// confirmation is preferred when it is already available when ctx is canceled.
func (r *Receipt) Wait(ctx context.Context) error {
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		select {
		case <-r.done:
			return r.err
		default:
			return ctx.Err()
		}
	}
}

// finish is called at most once, under the owning AckBuffer's lock.
func (r *Receipt) finish(err error) {
	r.err = err
	close(r.done)
}
