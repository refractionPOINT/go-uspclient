package uspclient

import (
	"sync"

	"github.com/refractionPOINT/go-uspclient/protocol"
)

const (
	// messageOverheadBytes is charged for every message on top of its
	// payload: the DataMessage struct, its pointer slot in the buffer and the
	// bookkeeping slot. It also guarantees every message has a non-zero size.
	messageOverheadBytes = 256
	// jsonEntryOverheadBytes approximates the cost of one map entry or slice
	// element of a decoded JSON payload (bucket slot, interface header, key
	// string header), excluding the bytes of keys and string values.
	jsonEntryOverheadBytes = 64
	// jsonScalarBytes approximates a boxed number / bool / nil value.
	jsonScalarBytes = 16
	// maxEstimateDepth bounds the recursion on nested JSON payloads. Anything
	// nested deeper is charged jsonDeepPenaltyBytes instead of being walked,
	// which also keeps a (malformed) cyclic map from looping forever.
	maxEstimateDepth     = 32
	jsonDeepPenaltyBytes = 4096
)

// ByteBudget is a process-wide pool of bytes that many clients draw from, so
// that a shared host running many clients has a hard overall bound on the
// memory held in unacknowledged messages regardless of how many clients exist
// or how fast any one of them is fed.
//
// Share one *ByteBudget between clients through AckBufferOptions.SharedBudget.
// The budget is a soft cap in exactly one way: a client that currently holds
// nothing is always allowed to admit one message, so a message larger than the
// whole budget cannot deadlock its client. The worst-case overshoot is
// therefore one message per client.
//
// A client's share is returned to the budget as its messages are acked and in
// full when the client is closed, so always Close() clients that use one.
// When the budget is full, blocked Ship calls are all woken on every release
// and race for the freed room; this only happens under saturation.
//
// A nil *ByteBudget is valid and means "no budget".
type ByteBudget struct {
	mu    sync.Mutex
	total uint64
	used  uint64
	// wake is closed and nilled on every release. It is created lazily by the
	// first reservation that fails, so a budget with no contention allocates
	// nothing per operation.
	wake chan struct{}
}

// NewByteBudget creates a budget of total bytes. total == 0 means unlimited
// (usage is still tracked).
func NewByteBudget(total uint64) *ByteBudget {
	return &ByteBudget{total: total}
}

// Total returns the size of the budget (0 = unlimited).
func (bb *ByteBudget) Total() uint64 {
	if bb == nil {
		return 0
	}
	return bb.total
}

// Used returns the bytes currently charged to the budget by all clients.
func (bb *ByteBudget) Used() uint64 {
	if bb == nil {
		return 0
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	return bb.used
}

// reserve tries to charge n bytes. force charges them even when over budget
// (used for the one-message-minimum guarantee). When it fails it returns a
// channel that is closed on the next release. The channel is obtained under
// the same lock as the failed attempt, so a release that happens right after
// the failure cannot be missed.
func (bb *ByteBudget) reserve(n uint64, force bool) (bool, <-chan struct{}) {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	// Written as a subtraction so a huge n cannot overflow the comparison.
	if force || bb.total == 0 || (n <= bb.total && bb.used <= bb.total-n) {
		bb.used += n
		return true, nil
	}
	if bb.wake == nil {
		bb.wake = make(chan struct{})
	}
	return false, bb.wake
}

// release returns n bytes and wakes everyone waiting for room.
func (bb *ByteBudget) release(n uint64) {
	if bb == nil || n == 0 {
		return
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	if n > bb.used {
		n = bb.used
	}
	bb.used -= n
	if bb.wake != nil {
		close(bb.wake)
		bb.wake = nil
	}
}

// estimateMessageSize returns a cheap estimate of the heap footprint of a
// message held in the buffer. It is deliberately not the wire size: what
// matters is the memory the process holds until the backend acknowledges it.
//
// The client only serializes a message later, on its sender goroutine, so
// there is no encoded form to reuse at admission time. Text, binary and bundle
// payloads are exact (their length). A JsonPayload is a decoded
// map[string]interface{}, whose footprint is dominated by per-entry overhead
// rather than by the JSON text, so it is estimated with one allocation-free
// walk of the map (see estimateJSON) instead of marshalling it, which would
// cost a full encode of every message on the hot path just to count bytes.
// The estimate errs on the high side.
func estimateMessageSize(m *protocol.DataMessage) uint64 {
	n := uint64(messageOverheadBytes)
	n += uint64(len(m.TextPayload) + len(m.BinaryPayload) +
		len(m.BundlePayload) + len(m.CompressedBundlePayload) +
		len(m.EventType) + len(m.ThisAtom) + len(m.ParentAtom) +
		len(m.TargetAtom) + len(m.InvestigationID))
	if len(m.JsonPayload) != 0 {
		n += estimateJSONMap(m.JsonPayload, 0)
	}
	return n
}

func estimateJSONMap(m map[string]interface{}, depth int) uint64 {
	if depth >= maxEstimateDepth {
		return jsonDeepPenaltyBytes
	}
	var n uint64
	for k, v := range m {
		n += jsonEntryOverheadBytes + uint64(len(k)) + estimateJSONValue(v, depth+1)
	}
	return n
}

func estimateJSONValue(v interface{}, depth int) uint64 {
	switch t := v.(type) {
	case string:
		return uint64(len(t))
	case map[string]interface{}:
		return estimateJSONMap(t, depth)
	case []interface{}:
		if depth >= maxEstimateDepth {
			return jsonDeepPenaltyBytes
		}
		var n uint64
		for _, e := range t {
			n += jsonEntryOverheadBytes + estimateJSONValue(e, depth+1)
		}
		return n
	case []string:
		var n uint64
		for _, s := range t {
			n += jsonEntryOverheadBytes + uint64(len(s))
		}
		return n
	case map[string]string:
		var n uint64
		for k, s := range t {
			n += jsonEntryOverheadBytes + uint64(len(k)) + uint64(len(s))
		}
		return n
	case []byte:
		return uint64(len(t))
	case nil, bool, float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return jsonScalarBytes
	default:
		// Unknown type (json.Number, custom types): charge a flat, generous
		// amount rather than reflecting on the hot path.
		return 2 * jsonEntryOverheadBytes
	}
}
