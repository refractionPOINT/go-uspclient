package uspclient

import (
	"container/heap"
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

// ByteBudget limits estimated unacknowledged bytes shared by clients. Always
// Close clients to return their charges. Without a pending ACK request, each
// client may admit one additional flagged message to avoid deadlock; Used
// includes these escape messages. A nil budget means unlimited.
//
// NewFairByteBudget also keeps a bounded newcomer reserve. Borrowers can use
// total-reserve bytes; the remainder admits only groups below their current
// fair share. This is newcomer runway, not an unconditional floor: parked,
// non-revocable messages can exhaust any finite reserve after enough arrivals.
// Progress beyond the one-message escape requires eligible bytes to be released.
// Waiters receive individual grants, with FIFO order inside each group and
// round-robin byte credits between groups. Large requests accumulate credits
// instead of losing every release to smaller requests.
type ByteBudget struct {
	mu                  sync.Mutex
	total, used, escape uint64
	fair                bool
	reserve, minGroup   uint64
	groups              map[string]*budgetGroup
	active              uint64
	legacy              budgetGroup
	waiting             list.List
	reserveReady        list.List
	blocked             groupHeap
}

// FairByteBudgetOptions configures bounded newcomer runway, not a permanent
// reservation per registered client. All state remains inside this process.
type FairByteBudgetOptions struct {
	// ReserveBytes defaults to total/8. It must be smaller than total.
	ReserveBytes uint64
	// MinGroupBytes is the minimum eligibility threshold inside the reserve;
	// zero defaults to the estimated overhead of one empty message (256 bytes).
	// It does not override the shared cap or guarantee that space is available.
	MinGroupBytes uint64
}

type budgetGroup struct {
	key                         string
	refs, used, escape, pending uint64
	active                      bool
	waiters                     list.List
	allElem, reserveElem        *list.Element
	heapIndex                   int
}

type budgetWaiter struct {
	n, credit uint64
	group     *budgetGroup
	wake      chan struct{}
	elem      *list.Element
	granted   bool
}

type groupHeap []*budgetGroup

func (h groupHeap) Len() int           { return len(h) }
func (h groupHeap) Less(i, j int) bool { return groupNeed(h[i]) < groupNeed(h[j]) }
func (h groupHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].heapIndex = i; h[j].heapIndex = j }
func (h *groupHeap) Push(v interface{}) {
	g := v.(*budgetGroup)
	g.heapIndex = len(*h)
	*h = append(*h, g)
}
func (h *groupHeap) Pop() interface{} {
	a := *h
	g := a[len(a)-1]
	a[len(a)-1] = nil
	*h = a[:len(a)-1]
	g.heapIndex = -1
	return g
}

// NewByteBudget creates an ungrouped budget. Zero means unlimited.
func NewByteBudget(total uint64) *ByteBudget {
	return &ByteBudget{total: total, legacy: budgetGroup{heapIndex: -1}}
}

// NewFairByteBudget creates an organization-aware shared budget. There is no
// group-count limit and registering another group cannot fail for lack of
// slots. Only groups holding bytes or waiters count toward the fair share.
func NewFairByteBudget(total uint64, o FairByteBudgetOptions) (*ByteBudget, error) {
	if o.ReserveBytes == 0 {
		o.ReserveBytes = total / 8
	}
	if (total == 0 && o.ReserveBytes != 0) || (total != 0 && o.ReserveBytes >= total) {
		return nil, fmt.Errorf("fair byte budget reserve must be smaller than total")
	}
	if o.MinGroupBytes == 0 {
		o.MinGroupBytes = messageOverheadBytes
	}
	bb := NewByteBudget(total)
	bb.fair = true
	bb.reserve = o.ReserveBytes
	bb.minGroup = o.MinGroupBytes
	bb.groups = make(map[string]*budgetGroup)
	return bb, nil
}

func (bb *ByteBudget) register(key string) (*budgetGroup, error) {
	if bb == nil || !bb.fair {
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
	g := &budgetGroup{key: key, refs: 1, heapIndex: -1}
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
	if !bb.fair || g == nil {
		return
	}
	g.refs--
	if g.refs == 0 {
		delete(bb.groups, g.key)
	}
}
func (bb *ByteBudget) group(g *budgetGroup) *budgetGroup {
	if g == nil {
		return &bb.legacy
	}
	return g
}

// Total returns the configured cap (zero means unlimited).
func (bb *ByteBudget) Total() uint64 {
	if bb == nil {
		return 0
	}
	return bb.total
}

// Used includes held messages and partial or complete waiter reservations.
func (bb *ByteBudget) Used() uint64 {
	if bb == nil {
		return 0
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	return bb.used
}

func (bb *ByteBudget) activateLocked(g *budgetGroup) {
	if !bb.fair {
		return
	}
	active := g.used != 0 || g.pending != 0
	if active != g.active {
		if active {
			bb.active++
		} else {
			bb.active--
		}
		g.active = active
	}
}
func (bb *ByteBudget) share(g *budgetGroup) uint64 {
	n := bb.active
	if !g.active {
		n++
	}
	if n < 2 {
		n = 2
	}
	share := bb.total / n
	if share < bb.minGroup {
		share = bb.minGroup
	}
	return share
}
func groupNeed(g *budgetGroup) uint64 {
	w := g.waiters.Front().Value.(*budgetWaiter)
	held := g.used
	remaining := w.n - w.credit
	if remaining > ^uint64(0)-held {
		return ^uint64(0)
	}
	return held + remaining
}
func (bb *ByteBudget) fits(n uint64, g *budgetGroup) bool {
	if bb.total == 0 {
		return true
	}
	used := bb.used - bb.escape
	if n > bb.total || used > bb.total-n {
		return false
	}
	if !bb.fair || used+n <= bb.total-bb.reserve {
		return true
	}
	share := bb.share(g)
	held := g.used
	return n <= share && held <= share-n
}
func (bb *ByteBudget) charge(n uint64, g *budgetGroup, escape bool) {
	if !bb.fair {
		bb.used += n
		return
	}
	g.used += n
	bb.used += n
	if escape {
		g.escape += n
		bb.escape += n
	}
	bb.activateLocked(g)
}
func (bb *ByteBudget) releaseLocked(n, escape uint64, g *budgetGroup) {
	if !bb.fair {
		if n > bb.used {
			n = bb.used
		}
		bb.used -= n
		return
	}
	if n > g.used {
		n = g.used
	}
	if n > bb.used {
		n = bb.used
	}
	g.used -= n
	bb.used -= n
	g.escape -= escape
	bb.escape -= escape
	bb.activateLocked(g)
}

// refreshLocked updates only this group's head. The heap lets reserve-only
// scheduling find newly eligible groups without rescanning every waiter.
func (bb *ByteBudget) refreshLocked(g *budgetGroup) {
	if g.waiters.Len() == 0 {
		if g.allElem != nil {
			bb.waiting.Remove(g.allElem)
			g.allElem = nil
		}
		if g.reserveElem != nil {
			bb.reserveReady.Remove(g.reserveElem)
			g.reserveElem = nil
		}
		if g.heapIndex >= 0 {
			heap.Remove(&bb.blocked, g.heapIndex)
		}
		return
	}
	if g.allElem == nil {
		g.allElem = bb.waiting.PushBack(g)
	}
	eligible := !bb.fair || groupNeed(g) <= bb.share(g)
	if eligible {
		if g.heapIndex >= 0 {
			heap.Remove(&bb.blocked, g.heapIndex)
		}
		if g.reserveElem == nil {
			g.reserveElem = bb.reserveReady.PushBack(g)
		}
	} else {
		if g.reserveElem != nil {
			bb.reserveReady.Remove(g.reserveElem)
			g.reserveElem = nil
		}
		if g.heapIndex < 0 {
			heap.Push(&bb.blocked, g)
		} else {
			heap.Fix(&bb.blocked, g.heapIndex)
		}
	}
}
func (bb *ByteBudget) nextReserveLocked() *budgetGroup {
	// Active-group departures can raise everyone's fair share. Promote only
	// newly eligible heap heads; decreases are checked lazily before use.
	for len(bb.blocked) != 0 {
		g := bb.blocked[0]
		if groupNeed(g) > bb.share(g) {
			break
		}
		heap.Pop(&bb.blocked)
		g.reserveElem = bb.reserveReady.PushBack(g)
	}
	for e := bb.reserveReady.Front(); e != nil; e = bb.reserveReady.Front() {
		g := e.Value.(*budgetGroup)
		if groupNeed(g) <= bb.share(g) {
			return g
		}
		bb.refreshLocked(g)
	}
	return nil
}

const budgetQuantum = 64 << 10

// scheduleLocked charges credits before waking their single owner. A large
// head keeps partial credits while other groups take their turns. With no
// eligible room it inspects group-head indexes, never all blocked Add calls.
func (bb *ByteBudget) scheduleLocked() {
	for bb.waiting.Len() != 0 {
		used := bb.used - bb.escape
		if bb.total != 0 && used >= bb.total {
			return
		}
		var g *budgetGroup
		base := uint64(0)
		if bb.total == 0 {
			base = ^uint64(0)
		} else if used < bb.total-bb.reserve {
			base = bb.total - bb.reserve - used
		}
		if base != 0 || !bb.fair {
			g = bb.waiting.Front().Value.(*budgetGroup)
		} else {
			g = bb.nextReserveLocked()
			if g == nil {
				return
			}
		}
		w := g.waiters.Front().Value.(*budgetWaiter)
		room := base
		if bb.total == 0 {
			room = ^uint64(0)
		} else if !bb.fair || groupNeed(g) <= bb.share(g) {
			room = bb.total - used
		}
		credit := w.n - w.credit
		if credit > budgetQuantum {
			credit = budgetQuantum
		}
		if credit > room {
			credit = room
		}
		if credit == 0 {
			return
		}
		bb.charge(credit, g, false)
		w.credit += credit
		if w.credit == w.n {
			g.waiters.Remove(w.elem)
			w.elem = nil
			w.granted = true
			close(w.wake)
		}
		bb.refreshLocked(g)
		if g.allElem != nil {
			bb.waiting.MoveToBack(g.allElem)
		}
		if g.reserveElem != nil {
			bb.reserveReady.MoveToBack(g.reserveElem)
		}
	}
}

func (bb *ByteBudget) removeWaiterLocked(w *budgetWaiter) {
	g := w.group
	if w.elem != nil {
		g.waiters.Remove(w.elem)
		w.elem = nil
	}
	if w.credit != 0 {
		bb.releaseLocked(w.credit, 0, g)
		w.credit = 0
	}
	g.pending--
	bb.activateLocked(g)
	bb.unregisterLocked(g)
	bb.refreshLocked(g)
}

func (bb *ByteBudget) reserveFor(n uint64, force bool, g *budgetGroup, w *budgetWaiter) (bool, bool, uint64, *budgetWaiter) {
	bb.mu.Lock()
	defer bb.mu.Unlock()
	g = bb.group(g)
	if w != nil && w.granted {
		g.pending--
		bb.activateLocked(g)
		bb.unregisterLocked(g)
		return true, false, bb.used, nil
	}
	if w != nil && force {
		bb.removeWaiterLocked(w)
		w = nil
	}
	if w != nil {
		return false, false, bb.used, w
	}
	fits := bb.fits(n, g)
	if (fits && g.waiters.Len() == 0) || force {
		escaped := bb.fair && (!fits || g.waiters.Len() != 0)
		bb.charge(n, g, escaped)
		if g.waiters.Len() != 0 {
			bb.refreshLocked(g)
		}
		return true, !fits || escaped, bb.used, nil
	}
	w = &budgetWaiter{n: n, group: g, wake: make(chan struct{})}
	w.elem = g.waiters.PushBack(w)
	g.pending++
	if bb.fair {
		g.refs++
	}
	bb.activateLocked(g)
	bb.refreshLocked(g)
	bb.scheduleLocked()
	return false, false, bb.used, w
}
func (bb *ByteBudget) cancel(w *budgetWaiter) {
	if bb == nil || w == nil {
		return
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	bb.removeWaiterLocked(w)
	bb.scheduleLocked()
}
func (bb *ByteBudget) release(n uint64) { bb.releaseFor(n, 0, nil) }
func (bb *ByteBudget) releaseFor(n, escape uint64, g *budgetGroup) {
	if bb == nil || n == 0 {
		return
	}
	bb.mu.Lock()
	defer bb.mu.Unlock()
	g = bb.group(g)
	bb.releaseLocked(n, escape, g)
	if g.waiters.Len() != 0 {
		bb.refreshLocked(g)
	}
	bb.scheduleLocked()
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
