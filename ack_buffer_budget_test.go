package uspclient

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/refractionPOINT/go-uspclient/protocol"
)

// textMsg builds a message whose estimated size is exactly
// messageOverheadBytes+n.
func textMsg(n int) *protocol.DataMessage {
	return &protocol.DataMessage{TextPayload: string(make([]byte, n))}
}

const testMsgPayload = 1000
const testMsgSize = messageOverheadBytes + testMsgPayload

func newBudgetBuffer(t *testing.T, o AckBufferOptions) *AckBuffer {
	t.Helper()
	b, err := NewAckBuffer(o)
	if err != nil {
		t.Fatal(err)
	}
	b.UpdateCapacity(1000)
	return b
}

// deliverAndAck simulates the backend: deliver n messages and ack the last.
func deliverAndAck(t *testing.T, b *AckBuffer, n int) {
	t.Helper()
	var last *protocol.DataMessage
	for i := 0; i < n; i++ {
		last = b.GetNextToDeliver(time.Second)
		if last == nil {
			t.Fatalf("message %d was not deliverable", i)
		}
	}
	if err := b.Ack(last.SeqNum); err != nil {
		t.Fatalf("Ack(%d): %v", last.SeqNum, err)
	}
}

// addAsync runs Add in a goroutine and reports its result.
func addAsync(b *AckBuffer, m *protocol.DataMessage, timeout time.Duration) <-chan bool {
	ch := make(chan bool, 1)
	go func() { ch <- b.Add(m, timeout) }()
	return ch
}

func expectBlocked(t *testing.T, ch <-chan bool, what string) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("%s: Add returned %v but should still be blocked", what, r)
	case <-time.After(150 * time.Millisecond):
	}
}

func expectResult(t *testing.T, ch <-chan bool, want bool, what string) {
	t.Helper()
	select {
	case r := <-ch:
		if r != want {
			t.Fatalf("%s: Add returned %v, want %v", what, r, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: blocked Add was never woken", what)
	}
}

func TestAckBufferPerClientByteCap(t *testing.T) {
	var pressure atomic.Int32
	b := newBudgetBuffer(t, AckBufferOptions{
		MaxBytes:       3 * testMsgSize,
		OnBackPressure: func() { pressure.Add(1) },
	})

	for i := 0; i < 3; i++ {
		if !b.Add(textMsg(testMsgPayload), time.Second) {
			t.Fatalf("message %d should fit under the cap", i)
		}
	}
	if got := b.BufferedBytes(); got != 3*testMsgSize {
		t.Fatalf("BufferedBytes = %d, want %d", got, 3*testMsgSize)
	}
	// The count window (1000) has plenty of room: only bytes can refuse this.
	if b.Add(textMsg(testMsgPayload), 100*time.Millisecond) {
		t.Fatal("4th message exceeds MaxBytes and must not be admitted")
	}
	if pressure.Load() == 0 {
		t.Fatal("OnBackPressure must fire when the byte cap blocks an Add")
	}

	blocked := addAsync(b, textMsg(testMsgPayload), time.Hour)
	expectBlocked(t, blocked, "byte cap")

	// Acking one message frees exactly one message worth of room.
	deliverAndAck(t, b, 1)
	expectResult(t, blocked, true, "after ack")
	if got := b.BufferedBytes(); got != 3*testMsgSize {
		t.Fatalf("BufferedBytes = %d after ack+add, want %d", got, 3*testMsgSize)
	}

	deliverAndAck(t, b, 3)
	if got := b.BufferedBytes(); got != 0 {
		t.Fatalf("BufferedBytes = %d after acking everything, want 0", got)
	}
}

func TestAckBufferOversizeMessageAdmittedWhenEmpty(t *testing.T) {
	pool := NewByteBudget(2 * testMsgSize)
	b := newBudgetBuffer(t, AckBufferOptions{MaxBytes: testMsgSize, SharedBudget: pool})

	huge := textMsg(50 * testMsgSize) // alone exceeds both the cap and the pool
	if !b.Add(huge, 100*time.Millisecond) {
		t.Fatal("an empty buffer must admit one message even if it exceeds every cap")
	}
	if got, want := pool.Used(), uint64(messageOverheadBytes+50*testMsgSize); got != want {
		t.Fatalf("oversize message must still be accounted: pool used %d, want %d", got, want)
	}
	if b.Add(textMsg(1), 100*time.Millisecond) {
		t.Fatal("nothing else fits while the oversize message is buffered")
	}

	deliverAndAck(t, b, 1)
	if pool.Used() != 0 || b.BufferedBytes() != 0 {
		t.Fatalf("oversize message not released on ack: pool=%d buffer=%d", pool.Used(), b.BufferedBytes())
	}
	if !b.Add(textMsg(1), 100*time.Millisecond) {
		t.Fatal("buffer must accept again once the oversize message is acked")
	}
}

// Two clients share a pool that fits two messages. When one client acks, the
// other client's blocked Add must wake up and be admitted.
func TestByteBudgetAckInOneClientWakesAnother(t *testing.T) {
	pool := NewByteBudget(2 * testMsgSize)
	a := newBudgetBuffer(t, AckBufferOptions{SharedBudget: pool})
	bb := newBudgetBuffer(t, AckBufferOptions{SharedBudget: pool})

	if !bb.Add(textMsg(testMsgPayload), time.Second) || !a.Add(textMsg(testMsgPayload), time.Second) {
		t.Fatal("first message of each client must fit")
	}
	if pool.Used() != 2*testMsgSize {
		t.Fatalf("pool used = %d, want %d", pool.Used(), 2*testMsgSize)
	}

	// Pool is full and client a is not empty: its second message must wait.
	blocked := addAsync(a, textMsg(testMsgPayload), time.Hour)
	expectBlocked(t, blocked, "full pool")

	// The other client acking is the only thing that can free room.
	deliverAndAck(t, bb, 1)
	expectResult(t, blocked, true, "ack in other client")
	if pool.Used() != 2*testMsgSize {
		t.Fatalf("pool used = %d, want %d", pool.Used(), 2*testMsgSize)
	}
}

func TestByteBudgetCloseOfOneClientWakesAnother(t *testing.T) {
	pool := NewByteBudget(2 * testMsgSize)
	a := newBudgetBuffer(t, AckBufferOptions{SharedBudget: pool})
	bb := newBudgetBuffer(t, AckBufferOptions{SharedBudget: pool})
	if !bb.Add(textMsg(testMsgPayload), time.Second) || !a.Add(textMsg(testMsgPayload), time.Second) {
		t.Fatal("setup adds failed")
	}

	blocked := addAsync(a, textMsg(testMsgPayload), time.Hour)
	expectBlocked(t, blocked, "full pool")

	bb.Close()
	expectResult(t, blocked, true, "close of other client")

	a.Close()
	if pool.Used() != 0 {
		t.Fatalf("a closed client must return its whole share: pool used = %d", pool.Used())
	}
}

// Close must wake an Add that is blocked, whatever it is blocked on, with or
// without a timeout, and make it fail promptly.
func TestAckBufferCloseWakesBlockedAdd(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) *AckBuffer
	}{
		{"count window", func(t *testing.T) *AckBuffer {
			b, _ := NewAckBuffer(AckBufferOptions{}) // window of 1
			if !b.Add(textMsg(1), time.Second) {
				t.Fatal("setup")
			}
			return b
		}},
		{"per-client bytes", func(t *testing.T) *AckBuffer {
			b := newBudgetBuffer(t, AckBufferOptions{MaxBytes: testMsgSize})
			if !b.Add(textMsg(testMsgPayload), time.Second) {
				t.Fatal("setup")
			}
			return b
		}},
		{"shared pool", func(t *testing.T) *AckBuffer {
			pool := NewByteBudget(testMsgSize)
			b := newBudgetBuffer(t, AckBufferOptions{SharedBudget: pool})
			if !b.Add(textMsg(testMsgPayload), time.Second) {
				t.Fatal("setup")
			}
			return b
		}},
	}
	for _, tc := range cases {
		for _, timeout := range []time.Duration{0, time.Hour} {
			t.Run(fmt.Sprintf("%s/timeout=%v", tc.name, timeout), func(t *testing.T) {
				b := tc.setup(t)
				blocked := addAsync(b, textMsg(testMsgPayload), timeout)
				expectBlocked(t, blocked, tc.name)
				start := time.Now()
				b.Close()
				expectResult(t, blocked, false, "Close")
				if d := time.Since(start); d > time.Second {
					t.Fatalf("Add took %v to return after Close", d)
				}
			})
		}
	}
}

// A message acked after Close (the drain case) must not release its bytes a
// second time: that would hand budget to other clients that is still in use.
func TestByteBudgetNoDoubleReleaseAfterClose(t *testing.T) {
	pool := NewByteBudget(10 * testMsgSize)
	a := newBudgetBuffer(t, AckBufferOptions{SharedBudget: pool})
	other := newBudgetBuffer(t, AckBufferOptions{SharedBudget: pool})
	for i := 0; i < 2; i++ {
		if !a.Add(textMsg(testMsgPayload), time.Second) || !other.Add(textMsg(testMsgPayload), time.Second) {
			t.Fatal("setup")
		}
	}
	a.Close()
	if pool.Used() != 2*testMsgSize {
		t.Fatalf("after Close pool used = %d, want only the other client's %d", pool.Used(), 2*testMsgSize)
	}
	deliverAndAck(t, a, 2) // drain after close
	if pool.Used() != 2*testMsgSize {
		t.Fatalf("ack after Close released again: pool used = %d, want %d", pool.Used(), 2*testMsgSize)
	}
	a.Close() // idempotent
	if pool.Used() != 2*testMsgSize {
		t.Fatalf("second Close released again: pool used = %d", pool.Used())
	}
	deliverAndAck(t, other, 2)
	if pool.Used() != 0 {
		t.Fatalf("pool used = %d after everything is acked, want 0", pool.Used())
	}
}

// A reconnect resends everything that is unacked. That must neither charge
// the messages again nor release them.
func TestByteBudgetResetDeliveryDoesNotRecount(t *testing.T) {
	pool := NewByteBudget(100 * testMsgSize)
	b := newBudgetBuffer(t, AckBufferOptions{SharedBudget: pool, MaxBytes: 100 * testMsgSize})
	for i := 0; i < 5; i++ {
		if !b.Add(textMsg(testMsgPayload), time.Second) {
			t.Fatal("setup")
		}
	}
	for round := 0; round < 3; round++ {
		for i := 0; i < 5; i++ {
			if b.GetNextToDeliver(time.Second) == nil {
				t.Fatalf("round %d: message %d not delivered", round, i)
			}
		}
		b.UpdateCapacity(1) // what Reconnect does
		b.ResetDelivery()
		if pool.Used() != 5*testMsgSize || b.BufferedBytes() != 5*testMsgSize {
			t.Fatalf("round %d: pool=%d buffer=%d, want %d", round, pool.Used(), b.BufferedBytes(), 5*testMsgSize)
		}
	}
	unacked, _ := b.GetUnAcked()
	if len(unacked) != 5 {
		t.Fatalf("unacked = %d, want 5", len(unacked))
	}
	deliverAndAck(t, b, 5)
	if pool.Used() != 0 {
		t.Fatalf("pool used = %d, want 0", pool.Used())
	}
}

// Many clients, many producers, random sizes, a tiny pool and a backend that
// acks continuously. Completion proves no wakeup is ever lost (every Add has
// no timeout), -race proves the locking, and the invariants prove the bound.
func TestByteBudgetStress(t *testing.T) {
	const (
		clients    = 8
		producers  = 4
		perProd    = 150
		maxPayload = 3000
	)
	pool := NewByteBudget(4 * (messageOverheadBytes + maxPayload))
	// Worst case overshoot is one message per client, see ByteBudget.
	hardLimit := pool.Total() + clients*(messageOverheadBytes+maxPayload)

	bufs := make([]*AckBuffer, clients)
	for i := range bufs {
		bufs[i] = newBudgetBuffer(t, AckBufferOptions{SharedBudget: pool})
		bufs[i].UpdateCapacity(64)
	}

	stopAck := make(chan struct{})
	var ackWG sync.WaitGroup
	var delivered atomic.Int64
	for _, b := range bufs {
		ackWG.Add(1)
		go func(b *AckBuffer) { // the "backend"
			defer ackWG.Done()
			for {
				select {
				case <-stopAck:
					return
				default:
				}
				m := b.GetNextToDeliver(5 * time.Millisecond)
				if m == nil {
					continue
				}
				if err := b.Ack(m.SeqNum); err != nil {
					t.Errorf("Ack: %v", err)
					return
				}
				delivered.Add(1)
			}
		}(b)
	}

	var maxUsed atomic.Uint64
	stopWatch := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopWatch:
				return
			default:
			}
			u := pool.Used()
			for {
				cur := maxUsed.Load()
				if u <= cur || maxUsed.CompareAndSwap(cur, u) {
					break
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	var prodWG sync.WaitGroup
	for ci, b := range bufs {
		for p := 0; p < producers; p++ {
			prodWG.Add(1)
			go func(b *AckBuffer, seed int64) {
				defer prodWG.Done()
				r := rand.New(rand.NewSource(seed))
				for i := 0; i < perProd; i++ {
					if !b.Add(textMsg(r.Intn(maxPayload)), 0) {
						t.Errorf("Add returned false on an open buffer")
						return
					}
				}
			}(b, int64(ci*100+p))
		}
	}
	done := make(chan struct{})
	go func() { prodWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("producers never finished: a wakeup was lost or the pool deadlocked")
	}

	want := int64(clients * producers * perProd)
	deadline := time.Now().Add(10 * time.Second)
	for delivered.Load() != want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(stopAck)
	close(stopWatch)
	ackWG.Wait()
	if got := delivered.Load(); got != want {
		t.Fatalf("delivered %d messages, want %d", got, want)
	}
	if pool.Used() != 0 {
		t.Fatalf("pool used = %d after everything was acked, want 0 (leak)", pool.Used())
	}
	if u := maxUsed.Load(); u > hardLimit {
		t.Fatalf("pool peaked at %d bytes, above the documented bound %d", u, hardLimit)
	}
	for i, b := range bufs {
		if b.BufferedBytes() != 0 {
			t.Fatalf("client %d still reports %d buffered bytes", i, b.BufferedBytes())
		}
	}
}

// Closing clients while producers are blocked on the pool must neither
// deadlock nor leak budget.
func TestByteBudgetStressCloseWhileBlocked(t *testing.T) {
	pool := NewByteBudget(3 * testMsgSize)
	const clients = 6
	bufs := make([]*AckBuffer, clients)
	var wg sync.WaitGroup
	for i := range bufs {
		bufs[i] = newBudgetBuffer(t, AckBufferOptions{SharedBudget: pool})
		for p := 0; p < 3; p++ {
			wg.Add(1)
			go func(b *AckBuffer) {
				defer wg.Done()
				for b.Add(textMsg(testMsgPayload), time.Hour) {
				}
			}(bufs[i])
		}
	}
	time.Sleep(100 * time.Millisecond) // nobody acks: everything piles up and blocks
	for _, b := range bufs {
		b.Close()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("producers still blocked after every client was closed")
	}
	if pool.Used() != 0 {
		t.Fatalf("pool used = %d after all clients closed, want 0", pool.Used())
	}
}

func TestEstimateMessageSize(t *testing.T) {
	t.Run("exact for opaque payloads", func(t *testing.T) {
		m := &protocol.DataMessage{
			TextPayload: "aaaa", BinaryPayload: "bb", BundlePayload: []byte("ccc"),
			CompressedBundlePayload: []byte("dddd"), EventType: "e", ThisAtom: "x",
		}
		if got, want := estimateMessageSize(m), uint64(messageOverheadBytes+4+2+3+4+1+1); got != want {
			t.Fatalf("estimate = %d, want %d", got, want)
		}
	})

	t.Run("json is never below its serialized size", func(t *testing.T) {
		payloads := []map[string]interface{}{
			{"a": "b"},
			{
				"event": map[string]interface{}{
					"EventID": float64(4624), "Computer": "host-01.example.test",
					"Data":   []interface{}{"x", "y", float64(1), true, nil},
					"Nested": map[string]interface{}{"k1": "v1", "k2": map[string]interface{}{"deep": "value"}},
				},
				"ts": float64(1700000000), "tags": []string{"a", "b", "c"},
			},
		}
		for i, p := range payloads {
			raw, _ := json.Marshal(p)
			got := estimateMessageSize(&protocol.DataMessage{JsonPayload: p}) - messageOverheadBytes
			if got < uint64(len(raw)) {
				t.Errorf("payload %d: estimate %d below serialized size %d", i, got, len(raw))
			}
		}
	})

	t.Run("cyclic and deeply nested json terminates", func(t *testing.T) {
		cyclic := map[string]interface{}{}
		cyclic["self"] = cyclic
		if got := estimateMessageSize(&protocol.DataMessage{JsonPayload: cyclic}); got == 0 {
			t.Fatal("empty estimate")
		}
		var deep interface{} = "leaf"
		for i := 0; i < 100000; i++ {
			deep = []interface{}{deep}
		}
		estimateMessageSize(&protocol.DataMessage{JsonPayload: map[string]interface{}{"d": deep}})
	})
}

func TestByteBudgetReserveHugeRequestDoesNotOverflow(t *testing.T) {
	pool := NewByteBudget(100)
	if ok, _, _, wake := pool.reserveFor(^uint64(0), false, nil, nil); ok || wake == nil {
		t.Fatalf("a request larger than the pool must wait, got ok=%v wake=%v", ok, wake)
	} else {
		pool.cancel(wake)
	}
	if ok, _, _, _ := pool.reserveFor(60, false, nil, nil); !ok {
		t.Fatal("60/100 must fit")
	}
	if ok, _, _, waiter := pool.reserveFor(41, false, nil, nil); ok {
		t.Fatal("101/100 must not fit")
	} else {
		pool.cancel(waiter)
	}
	if ok, overshot, _, _ := pool.reserveFor(500, true, nil, nil); !ok || !overshot {
		t.Fatalf("a forced reserve must succeed and report the overshoot, got ok=%v overshot=%v", ok, overshot)
	}
	pool.release(1000) // over-release is clamped, never wraps
	if pool.Used() != 0 {
		t.Fatalf("used = %d, want 0", pool.Used())
	}
	unlimited := NewByteBudget(0)
	if ok, _, _, _ := unlimited.reserveFor(1<<40, false, nil, nil); !ok {
		t.Fatal("a zero-size budget means unlimited")
	}
}

// The no-budget path (every existing caller) must keep costing nothing extra:
// no allocation in the steady-state add/deliver/ack cycle.
func TestAckBufferNoBudgetPathDoesNotAllocate(t *testing.T) {
	b, _ := NewAckBuffer(AckBufferOptions{})
	b.UpdateCapacity(64)
	m := &protocol.DataMessage{TextPayload: "payload", JsonPayload: map[string]interface{}{"k": "v"}}
	allocs := testing.AllocsPerRun(1000, func() {
		if !b.Add(m, time.Second) {
			t.Fatal("Add")
		}
		n := b.GetNextToDeliver(time.Second)
		if err := b.Ack(n.SeqNum); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("no-budget add/deliver/ack cycle allocates %v times, want 0", allocs)
	}
	if b.BufferedBytes() != 0 {
		t.Fatal("no-budget buffers do not track bytes")
	}
}

func benchmarkAddAck(b *testing.B, o AckBufferOptions, mk func() *protocol.DataMessage) {
	buf, _ := NewAckBuffer(o)
	buf.UpdateCapacity(1024)
	m := mk()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !buf.Add(m, time.Second) {
			b.Fatal("Add")
		}
		n := buf.GetNextToDeliver(time.Second)
		if err := buf.Ack(n.SeqNum); err != nil {
			b.Fatal(err)
		}
	}
}

func jsonMsg() *protocol.DataMessage {
	return &protocol.DataMessage{JsonPayload: map[string]interface{}{
		"event": map[string]interface{}{"id": float64(1), "host": "host-01", "data": []interface{}{"a", "b"}},
		"ts":    float64(1700000000),
	}}
}

func BenchmarkAckBufferNoBudget(b *testing.B) {
	benchmarkAddAck(b, AckBufferOptions{}, jsonMsg)
}

func BenchmarkAckBufferWithBudgetJSON(b *testing.B) {
	benchmarkAddAck(b, AckBufferOptions{SharedBudget: NewByteBudget(1 << 30), MaxBytes: 1 << 30}, jsonMsg)
}

func BenchmarkAckBufferWithBudgetText(b *testing.B) {
	benchmarkAddAck(b, AckBufferOptions{SharedBudget: NewByteBudget(1 << 30)}, func() *protocol.DataMessage {
		return &protocol.DataMessage{TextPayload: "a log line of a typical length, perhaps a hundred bytes or so, give or take a few"}
	})
}

// Many clients hammering one shared budget well below its limit: the shared
// lock must not become a throughput cliff.
func BenchmarkByteBudgetContended(b *testing.B) {
	pool := NewByteBudget(1 << 40)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		buf, _ := NewAckBuffer(AckBufferOptions{SharedBudget: pool})
		buf.UpdateCapacity(1024)
		m := jsonMsg()
		for pb.Next() {
			buf.Add(m, time.Second)
			n := buf.GetNextToDeliver(time.Second)
			buf.Ack(n.SeqNum)
		}
	})
}

// heldFlagged reports whether the buffer holds an unacked message that carries
// an ack request, and the highest such sequence number.
func heldFlagged(t *testing.T, b *AckBuffer) (bool, uint64) {
	t.Helper()
	held, _ := b.GetUnAcked()
	var seq uint64
	for _, m := range held {
		if m.AckRequested {
			seq = m.SeqNum
		}
	}
	return seq != 0, seq
}

// backendAck behaves like the backend: it only acknowledges messages that were
// flagged with AckRequested (everything up to the highest flagged one that has
// been delivered), never proactively.
func backendAck(t *testing.T, b *AckBuffer) {
	t.Helper()
	var highest uint64
	for {
		m := b.GetNextToDeliver(0)
		if m == nil {
			break
		}
		if m.AckRequested {
			highest = m.SeqNum
		}
	}
	if highest != 0 {
		if err := b.Ack(highest); err != nil {
			t.Fatalf("Ack(%d): %v", highest, err)
		}
	}
}

// The count-based rule only asks for an ack every ackEvery messages (tens of
// thousands with a big window), and the backend only acks on request. A client
// that blocks on bytes long before that must already have a request in flight,
// or it stalls until the backend's slow periodic ack. Property: whenever Add
// gives up on bytes, the buffer holds a flagged message.
func TestByteBudgetBlockedClientHasAckRequestPending(t *testing.T) {
	const maxPayload = 300000
	configs := []struct {
		name  string
		opts  func() []AckBufferOptions
		nbufs int
	}{
		{"per-client cap", func() []AckBufferOptions {
			return []AckBufferOptions{{MaxBytes: 1 << 20}}
		}, 1},
		{"shared pool", func() []AckBufferOptions {
			pool := NewByteBudget(1 << 20)
			return []AckBufferOptions{{SharedBudget: pool}, {SharedBudget: pool}, {SharedBudget: pool}}
		}, 3},
		{"both", func() []AckBufferOptions {
			pool := NewByteBudget(1 << 20)
			return []AckBufferOptions{{SharedBudget: pool, MaxBytes: 600000}, {SharedBudget: pool, MaxBytes: 600000}}
		}, 2},
	}
	for _, cfg := range configs {
		t.Run(cfg.name, func(t *testing.T) {
			var bufs []*AckBuffer
			for _, o := range cfg.opts() {
				b := newBudgetBuffer(t, o)
				b.UpdateCapacity(100000) // ackEvery = 50000: the count rule never fires here
				bufs = append(bufs, b)
			}
			r := rand.New(rand.NewSource(1))
			blocks := 0
			for i := 0; i < 1200; i++ {
				b := bufs[r.Intn(len(bufs))]
				if b.Add(textMsg(1+r.Intn(maxPayload)), 5*time.Millisecond) {
					if r.Intn(40) == 0 { // acks arrive sporadically
						backendAck(t, b)
					}
					continue
				}
				blocks++
				if ok, _ := heldFlagged(t, b); !ok {
					t.Fatalf("iteration %d: Add blocked on bytes while the buffer holds no ack request", i)
				}
				// Every blocked client gets released by its own requested ack.
				backendAck(t, b)
			}
			if blocks == 0 {
				t.Fatal("the scenario never hit the byte limit, test proves nothing")
			}
		})
	}
}

// A client that was admitted without a flag while the pool had room, and then
// finds the pool filled by others, must still get a request in flight rather
// than wait for the periodic ack.
func TestByteBudgetUnflaggedTailGetsRequestWhenPoolFills(t *testing.T) {
	pool := NewByteBudget(8 * testMsgSize)
	a := newBudgetBuffer(t, AckBufferOptions{SharedBudget: pool})
	other := newBudgetBuffer(t, AckBufferOptions{SharedBudget: pool})
	a.UpdateCapacity(100000)
	other.UpdateCapacity(100000)

	if !a.Add(textMsg(testMsgPayload), time.Second) {
		t.Fatal("setup")
	}
	if ok, _ := heldFlagged(t, a); ok {
		t.Fatal("a message admitted into a mostly empty pool must not ask for an ack")
	}
	for i := 0; i < 7; i++ { // fill the pool from the other client
		if !other.Add(textMsg(testMsgPayload), time.Second) {
			t.Fatalf("other client message %d", i)
		}
	}

	if !a.Add(textMsg(testMsgPayload), 100*time.Millisecond) {
		t.Fatal("a client holding no ack request must be admitted (and flagged) even when the pool is full")
	}
	if ok, _ := heldFlagged(t, a); !ok {
		t.Fatal("the forced message must carry AckRequested")
	}
	blocked := addAsync(a, textMsg(testMsgPayload), time.Hour)
	expectBlocked(t, blocked, "client with an ack request pending")

	backendAck(t, a)
	expectResult(t, blocked, true, "after the requested ack")
}

// Under normal load (lots of room) the byte budget must not add ack traffic.
func TestByteBudgetNoExtraAckRequestsWhenNotUnderPressure(t *testing.T) {
	pool := NewByteBudget(1 << 30)
	b := newBudgetBuffer(t, AckBufferOptions{SharedBudget: pool, MaxBytes: 1 << 29})
	b.UpdateCapacity(100000)
	for i := 0; i < 5000; i++ {
		if !b.Add(textMsg(testMsgPayload), time.Second) {
			t.Fatal("Add")
		}
	}
	held, _ := b.GetUnAcked()
	for _, m := range held {
		if m.AckRequested {
			t.Fatalf("message %d asked for an ack with the budget nearly empty", m.SeqNum)
		}
	}
}

// Under pressure the buffer asks for acks about every 1/16th of the limit, not
// for every message: small messages must not turn into one ack per message.
func TestByteBudgetPressureAckRequestsAreSparse(t *testing.T) {
	const limit = 1 << 20
	b := newBudgetBuffer(t, AckBufferOptions{MaxBytes: limit})
	b.UpdateCapacity(100000)
	added := 0
	for b.Add(textMsg(100), 5*time.Millisecond) {
		added++
	}
	held, _ := b.GetUnAcked()
	flagged := 0
	for _, m := range held {
		if m.AckRequested {
			flagged++
		}
	}
	// Half the limit is calm, the other half asks about every limit/16.
	if added < 2000 || flagged == 0 || flagged > 20 {
		t.Fatalf("added %d small messages with %d ack requests, want thousands of messages and ~8 requests", added, flagged)
	}
	if ok, _ := heldFlagged(t, b); !ok {
		t.Fatal("blocked without any ack request held")
	}
}
