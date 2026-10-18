package uspclient

import (
	"container/list"
	"fmt"
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

// ByteBudget bounds the estimated bytes held by clients until acknowledgment.
// Share it through AckBufferOptions.SharedBudget and always Close clients.
// A client without a pending ACK request may admit one extra flagged message
// to avoid deadlock, so the cap can be exceeded by one message per client.
// A nil budget means unlimited. Blocked additions use individual wakeups.
//
// NewByteBudget provides a shared cap. NewFairByteBudget also protects an equal
// floor for each BudgetGroup, including groups that join after others fill the
// pool. Non-revocable unacknowledged messages cannot be reclaimed from borrowers,
// so floors are reserved for a bounded number of groups before lending bytes.
// Half the pool is reserved for floors; the remainder can be borrowed by any
// group. Groups with no traffic keep their floor protected until Close.
// A group's adapters share its floor rather than each receiving a floor.
type ByteBudget struct {
	mu     sync.Mutex
	total  uint64
	used   uint64
	escape uint64
	slots  uint32
	floor  uint64
	groups map[string]*budgetGroup
	// protected is the sum of unused floors, including unregistered slots.
	protected uint64
	waiters   list.List
}

type budgetGroup struct {
	key    string
	refs   uint64
	used   uint64
	escape uint64
}

type budgetWaiter struct {
	n       uint64
	group   *budgetGroup
	wake    chan struct{}
	elem    *list.Element
	granted bool
}

// NewByteBudget creates an ungrouped budget; zero means unlimited.
func NewByteBudget(total uint64) *ByteBudget { return &ByteBudget{total: total} }

// NewFairByteBudget reserves total/(2*groupSlots) bytes for each group. The
// remaining bytes can be borrowed. groupSlots bounds simultaneously registered
// groups, not clients; NewAckBuffer fails when a new group exceeds that bound.
// total must be at least 2*groupSlots and groupSlots must be positive.
func NewFairByteBudget(total uint64, groupSlots uint32) (*ByteBudget, error) {
	if groupSlots == 0 || total/uint64(groupSlots) < 2 {
		return nil, fmt.Errorf("fair byte budget requires positive slots and at least 2 bytes per slot")
	}
	floor := total / (2 * uint64(groupSlots))
	return &ByteBudget{total: total, slots: groupSlots, floor: floor,
		protected: floor * uint64(groupSlots), groups: make(map[string]*budgetGroup)}, nil
}

func (bb *ByteBudget) register(key string) (*budgetGroup, error) {
	if bb == nil || bb.slots == 0 {
		return nil, nil
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	if key == "" {
		return nil, fmt.Errorf("fair byte budget requires BudgetGroup")
	}
	if g := bb.groups[key]; g != nil {
		g.refs++
		return g, nil
	}
	if uint32(len(bb.groups)) >= bb.slots {
		return nil, fmt.Errorf("fair byte budget group capacity reached (%d)", bb.slots)
	}
	g := &budgetGroup{key: key, refs: 1}
	bb.groups[key] = g
	return g, nil
}

func (bb *ByteBudget) unregister(g *budgetGroup) {
	if bb == nil || g == nil {
		return
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	bb.unregisterLocked(g)
}

func (bb *ByteBudget) unregisterLocked(g *budgetGroup) {
	if g == nil {
		return
	}
	g.refs--
	if g.refs == 0 {
		delete(bb.groups, g.key)
	}
}

// Total returns the budget size (zero means unlimited).
func (bb *ByteBudget) Total() uint64 {
	if bb == nil {
		return 0
	}
	return bb.total
}

// Used returns all charged bytes, including reservations granted to waiters.
func (bb *ByteBudget) Used() uint64 {
	if bb == nil {
		return 0
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	return bb.used
}

func (bb *ByteBudget) unusedFloor(g *budgetGroup) uint64 {
	if g == nil || g.used-g.escape >= bb.floor {
		return 0
	}
	return bb.floor - (g.used - g.escape)
}

func (bb *ByteBudget) fits(n uint64, g *budgetGroup) bool {
	if bb.total == 0 {
		return true
	}
	// Only this group's own unused floor may be consumed. Written with
	// subtractions to keep oversized requests from overflowing comparisons.
	available := bb.total - bb.protected + bb.unusedFloor(g)
	return n <= available && bb.used-bb.escape <= available-n
}

func (bb *ByteBudget) charge(n uint64, g *budgetGroup, escape bool) {
	if g != nil {
		before := bb.unusedFloor(g)
		if escape {
			g.escape += n
		}
		g.used += n
		bb.protected -= before - bb.unusedFloor(g)
	}
	if escape {
		bb.escape += n
	}
	bb.used += n
}

func (bb *ByteBudget) releaseLocked(n, escape uint64, g *budgetGroup) {
	if g != nil {
		if n > g.used {
			n = g.used
		}
		before := bb.unusedFloor(g)
		g.used -= n
		g.escape -= escape
		bb.protected += bb.unusedFloor(g) - before
	}
	if n > bb.used {
		n = bb.used
	}
	bb.used -= n
	bb.escape -= escape
}

// reserveFor charges immediately or enqueues one cancellable waiter for Add.
func (bb *ByteBudget) reserveFor(n uint64, force bool, g *budgetGroup, w *budgetWaiter) (bool, bool, uint64, *budgetWaiter) {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	if w != nil && w.granted {
		bb.unregisterLocked(g)
		return true, false, bb.used, nil
	}
	fits := bb.fits(n, g)
	if fits || force {
		if w != nil {
			bb.waiters.Remove(w.elem)
			w.elem = nil
			bb.unregisterLocked(g)
		}
		bb.charge(n, g, !fits && bb.slots != 0)
		return true, !fits, bb.used, nil
	}
	if w == nil {
		w = &budgetWaiter{n: n, group: g, wake: make(chan struct{})}
		w.elem = bb.waiters.PushBack(w)
		if g != nil {
			g.refs++
		}
	}
	return false, false, bb.used, w
}

// grantLocked reserves bytes before waking only eligible waiters. A newly
// arriving producer cannot steal a notified waiter's reservation. Requests
// that do not fit do not prevent another group's protected floor from moving.
func (bb *ByteBudget) grantLocked() {
	for e := bb.waiters.Front(); e != nil; {
		next := e.Next()
		w := e.Value.(*budgetWaiter)
		if bb.fits(w.n, w.group) {
			bb.waiters.Remove(e)
			w.elem = nil
			bb.charge(w.n, w.group, false)
			w.granted = true
			close(w.wake)
		}
		e = next
	}
}

func (bb *ByteBudget) cancel(w *budgetWaiter) {
	if bb == nil || w == nil {
		return
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	if w.granted {
		bb.releaseLocked(w.n, 0, w.group)
	} else if w.elem != nil {
		bb.waiters.Remove(w.elem)
	}
	bb.unregisterLocked(w.group)
	bb.grantLocked()
}

func (bb *ByteBudget) release(n uint64) { bb.releaseFor(n, 0, nil) }

func (bb *ByteBudget) releaseFor(n, escape uint64, g *budgetGroup) {
	if bb == nil || n == 0 {
		return
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	bb.releaseLocked(n, escape, g)
	bb.grantLocked()
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
