package uspclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/refractionPOINT/go-uspclient/protocol"
)

func fairPool(t *testing.T, frames, reserve uint64) *ByteBudget {
	t.Helper()
	p, err := NewFairByteBudget(frames*testMsgSize, FairByteBudgetOptions{ReserveBytes: reserve * testMsgSize})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func groupedBuffer(t *testing.T, p *ByteBudget, key string) *AckBuffer {
	t.Helper()
	b := newBudgetBuffer(t, AckBufferOptions{SharedBudget: p, BudgetGroup: key})
	t.Cleanup(b.Close)
	return b
}
func flaggedTestMessage() *protocol.DataMessage {
	m := textMsg(testMsgPayload)
	m.AckRequested = true
	return m
}

// A parks before B registers, using every lendable byte via four clients.
// B can use the bounded newcomer runway repeatedly without an ACK from A.
func TestFairByteBudgetParkedTenantCannotConsumeNewTenantRunway(t *testing.T) {
	p := fairPool(t, 64, 8)
	var parked []*AckBuffer
	for i := 0; i < 4; i++ {
		parked = append(parked, groupedBuffer(t, p, "tenant-a"))
	}
	admitted := 0
	for ; admitted < 128; admitted++ {
		if !parked[admitted%4].Add(flaggedTestMessage(), 10*time.Millisecond) {
			break
		}
	}
	if admitted == 128 {
		t.Fatal("parked tenant has no bound")
	}
	healthy := groupedBuffer(t, p, "tenant-b")
	for round := 0; round < 20; round++ {
		for i := 0; i < 8; i++ {
			if !healthy.Add(flaggedTestMessage(), time.Second) {
				t.Fatalf("healthy tenant stalled at round=%d frame=%d", round, i)
			}
		}
		if p.Used() != 64*testMsgSize {
			t.Fatalf("used=%d want%d", p.Used(), 64*testMsgSize)
		}
		deliverAndAck(t, healthy, 8)
	}
	if admitted != 56 {
		t.Fatalf("borrower admitted%d want56", admitted)
	}
	if got, _ := parked[0].GetUnAcked(); len(got) != 14 {
		t.Fatal("parked tail changed")
	}
}

func TestFairByteBudgetEscapePreservesNewcomerRunway(t *testing.T) {
	p := fairPool(t, 64, 8)
	a := groupedBuffer(t, p, "tenant-a")
	for i := 0; i < 56; i++ {
		if !a.Add(flaggedTestMessage(), time.Second) {
			t.Fatal(i)
		}
	}
	escape := groupedBuffer(t, p, "tenant-a")
	if !escape.Add(textMsg(100*testMsgSize), time.Second) {
		t.Fatal("oversized first message must escape")
	}
	b := groupedBuffer(t, p, "tenant-b")
	for i := 0; i < 8; i++ {
		if !b.Add(flaggedTestMessage(), time.Second) {
			t.Fatalf("newcomer%d", i)
		}
	}
	escaped, _ := escape.GetUnAcked()
	if p.Used() != 64*testMsgSize+estimateMessageSize(escaped[0]) {
		t.Fatal("escape accounting")
	}
	deliverAndAck(t, escape, 1)
	escape.Close()
	if p.Used() != 64*testMsgSize {
		t.Fatal("escape returned another tenant's bytes")
	}
}

// A finite reserve cannot guarantee immediate room for every below-share
// group: A holds7/8, B fills1/8, then B remains below its share but has no room.
// C still gets its per-client escape and a fair turn when eligible bytes return.
func TestFairByteBudgetFiniteRunwayExhaustionAndReleasedProgress(t *testing.T) {
	p := fairPool(t, 64, 8)
	a := groupedBuffer(t, p, "tenant-a")
	for i := 0; i < 56; i++ {
		if !a.Add(flaggedTestMessage(), time.Second) {
			t.Fatal(i)
		}
	}
	b := groupedBuffer(t, p, "tenant-b")
	for i := 0; i < 8; i++ {
		if !b.Add(flaggedTestMessage(), time.Second) {
			t.Fatal(i)
		}
	}
	if b.Add(flaggedTestMessage(), 10*time.Millisecond) {
		t.Fatal("finite reserve was double spent")
	}
	c := groupedBuffer(t, p, "tenant-c")
	if !c.Add(flaggedTestMessage(), time.Second) {
		t.Fatal("newcomer lost one-frame escape")
	}
	if c.Add(flaggedTestMessage(), 10*time.Millisecond) {
		t.Fatal("newcomer has no normal runway left")
	}
	done := addAsync(c, flaggedTestMessage(), time.Second)
	// Ensure C has queued before releasing B's credits.
	waitFor(t, "third group waiter", func() bool { p.mu.Lock(); defer p.mu.Unlock(); return c.group.pending != 0 })
	deliverAndAck(t, b, 2)
	expectResult(t, done, true, "third group released-byte turn")
	if c.BufferedBytes() != 2*testMsgSize {
		t.Fatal("third group failed to progress beyond escape")
	}
	if a.BufferedBytes() != 56*testMsgSize {
		t.Fatal("parked bytes were revoked")
	}
}

func assertBudgetEmpty(t *testing.T, p *ByteBudget) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used != 0 || p.escape != 0 || p.active != 0 || p.waiting.Len() != 0 || p.reserveReady.Len() != 0 || len(p.blocked) != 0 || len(p.groups) != 0 {
		t.Fatalf("budget leak: used%d escape%d active%d waiting%d reserve%d blocked%d groups%d", p.used, p.escape, p.active, p.waiting.Len(), p.reserveReady.Len(), len(p.blocked), len(p.groups))
	}
}

// Idle registrations neither consume a share nor impose a creation limit.
// A thousand simultaneous groups remain bounded by T plus one escape/client;
// bookkeeping grows with actual clients, not a preallocated group table.
func TestFairByteBudgetThousandGroupsNoAdmissionFailureAndBoundedMemory(t *testing.T) {
	const groups = 1250
	p := fairPool(t, 64, 8)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	buffers := make([]*AckBuffer, 0, groups)
	for i := 0; i < groups; i++ {
		b, err := NewAckBuffer(AckBufferOptions{SharedBudget: p, BudgetGroup: fmt.Sprint(i)})
		if err != nil {
			t.Fatalf("group%d creation: %v", i, err)
		}
		buffers = append(buffers, b)
	}
	if p.active != 0 {
		t.Fatal("idle registrations count as active")
	}
	for _, b := range buffers {
		if !b.Add(flaggedTestMessage(), time.Second) {
			t.Fatal("first-frame progress failed")
		}
	}
	runtime.ReadMemStats(&after)
	p.mu.Lock()
	normal, escape, active := p.used-p.escape, p.escape, p.active
	p.mu.Unlock()
	if normal > p.total || escape > groups*testMsgSize || active != groups {
		t.Fatalf("unbounded normal%d escape%d active%d", normal, escape, active)
	}
	if delta := after.TotalAlloc - before.TotalAlloc; delta > groups*16*1024 {
		t.Fatalf("group bookkeeping allocated%d bytes", delta)
	}
	for _, b := range buffers {
		b.Close()
	}
	assertBudgetEmpty(t, p)
}

func TestFairByteBudgetOptionsAndActiveLifecycle(t *testing.T) {
	if _, err := NewFairByteBudget(100, FairByteBudgetOptions{ReserveBytes: 100}); err == nil {
		t.Fatal("invalid reserve accepted")
	}
	p, err := NewFairByteBudget(1024, FairByteBudgetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.reserve != 128 {
		t.Fatal("default reserve is not1/8")
	}
	if _, err := NewAckBuffer(AckBufferOptions{SharedBudget: p}); err == nil {
		t.Fatal("missing trusted group accepted")
	}
	b := groupedBuffer(t, p, "tenant-a")
	sibling := groupedBuffer(t, p, "tenant-a")
	if p.active != 0 || len(p.groups) != 1 {
		t.Fatal("idle siblings counted")
	}
	if !b.Add(flaggedTestMessage(), time.Second) {
		t.Fatal("first frame")
	}
	if p.active != 1 {
		t.Fatal("held group not active")
	}
	deliverAndAck(t, b, 1)
	if p.active != 0 {
		t.Fatal("empty live client still active")
	}
	b.Close()
	sibling.Close()
	assertBudgetEmpty(t, p)
}

// Byte credits reserve released memory for the large FIFO head. Smaller
// requests in the same group cannot consume its credits or wake prematurely.
func TestFairByteBudgetIndividualWaiterGrantsAndCancellation(t *testing.T) {
	// Keep this FIFO/credit test independent of newcomer reserve eligibility.
	p, err := NewFairByteBudget(10, FairByteBudgetOptions{MinGroupBytes: 20})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := p.register("shared")
	p.reserveFor(10, false, g, nil)
	_, _, _, large := p.reserveFor(8, false, g, nil)
	_, _, _, small := p.reserveFor(2, false, g, nil)
	p.releaseFor(2, 0, g)
	select {
	case <-large.wake:
		t.Fatal("incomplete large waiter woken")
	default:
	}
	select {
	case <-small.wake:
		t.Fatal("small waiter bypassed group FIFO")
	default:
	}
	if large.credit != 2 || small.credit != 0 || p.Used() != 10 {
		t.Fatal("released bytes were not escrowed")
	}
	p.releaseFor(6, 0, g)
	select {
	case <-large.wake:
	default:
		t.Fatal("large grant incomplete")
	}
	if ok, _, _, late := p.reserveFor(1, false, g, nil); ok {
		t.Fatal("late producer stole grant")
	} else {
		p.cancel(late)
	}
	p.cancel(large)
	select {
	case <-small.wake:
	default:
		t.Fatal("cancelled grant not reassigned")
	}
	ok, _, _, next := p.reserveFor(2, false, g, small)
	if !ok || next != nil || p.Used() != 4 {
		t.Fatalf("consuming grant charged twice: used%d", p.Used())
	}
	p.releaseFor(4, 0, g)
	p.unregister(g)
	assertBudgetEmpty(t, p)
}

func TestFairByteBudgetLargeWaiterCannotStarveBehindSmallGroup(t *testing.T) {
	p, err := NewFairByteBudget(1000, FairByteBudgetOptions{ReserveBytes: 125, MinGroupBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := p.register("parked")
	largeGroup, _ := p.register("large")
	smallGroup, _ := p.register("small")
	if ok, _, _, _ := p.reserveFor(875, false, a, nil); !ok {
		t.Fatal("base fill")
	}
	if ok, _, _, _ := p.reserveFor(125, false, smallGroup, nil); !ok {
		t.Fatal("runway fill")
	}
	_, _, _, large := p.reserveFor(100, false, largeGroup, nil)
	_, _, _, small := p.reserveFor(1, false, smallGroup, nil)
	releases := 0
	for ; releases < 250; releases++ {
		p.releaseFor(1, 0, smallGroup)
		select {
		case <-large.wake:
			goto granted
		default:
		}
		select {
		case <-small.wake:
			if ok, _, _, _ := p.reserveFor(1, false, smallGroup, small); !ok {
				t.Fatal("small grant not consumable")
			}
			_, _, _, small = p.reserveFor(1, false, smallGroup, nil)
		default:
		}
	}
	t.Fatal("large waiter starved")
granted:
	if large.credit != 100 {
		t.Fatal("large request woke incomplete")
	}
	if releases > 200 {
		t.Fatalf("large request waited%d turns", releases)
	}
	p.cancel(small)
	if ok, _, _, _ := p.reserveFor(100, false, largeGroup, large); !ok {
		t.Fatal("large grant not consumable")
	}
	p.releaseFor(100, 0, largeGroup)
	p.releaseFor(875, 0, a)
	p.releaseFor(smallGroup.used, 0, smallGroup)
	p.unregister(a)
	p.unregister(largeGroup)
	p.unregister(smallGroup)
	assertBudgetEmpty(t, p)
}

func TestFairByteBudgetPendingGrantKeepsGroupRegistered(t *testing.T) {
	p := fairPool(t, 8, 1)
	g, _ := p.register("tenant-a")
	p.reserveFor(7*testMsgSize, false, g, nil)
	_, _, _, w := p.reserveFor(testMsgSize, false, g, nil)
	p.releaseFor(testMsgSize, 0, g)
	p.releaseFor(6*testMsgSize, 0, g)
	p.unregister(g)
	if p.groups["tenant-a"] != g {
		t.Fatal("pending grant lost its group")
	}
	other, err := p.register("tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	p.unregister(other)
	p.cancel(w)
	assertBudgetEmpty(t, p)
}

func TestFairByteBudgetConcurrentTenantLifecycle(t *testing.T) {
	p := fairPool(t, 32, 4)
	var wg sync.WaitGroup
	for tenant := 0; tenant < 8; tenant++ {
		wg.Add(1)
		go func(tenant int) {
			defer wg.Done()
			for round := 0; round < 20; round++ {
				b, err := NewAckBuffer(AckBufferOptions{SharedBudget: p, BudgetGroup: fmt.Sprint(tenant)})
				if err != nil {
					t.Error(err)
					return
				}
				b.UpdateCapacity(100)
				for i := 0; i < 8; i++ {
					b.Add(flaggedTestMessage(), time.Microsecond)
				}
				done := make(chan struct{})
				go func() { defer close(done); b.Add(flaggedTestMessage(), time.Second) }()
				b.Close()
				<-done
			}
		}(tenant)
	}
	wg.Wait()
	assertBudgetEmpty(t, p)
}

func TestFairByteBudgetFailedClientCreationReleasesGroup(t *testing.T) {
	p := fairPool(t, 8, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	for i := 0; i < 3; i++ {
		_, err := NewClient(context.Background(), ClientOptions{Identity: Identity{Oid: uuid.NewString(), InstallationKey: "iid"}, DestURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/usp", BufferOptions: AckBufferOptions{SharedBudget: p, BudgetGroup: fmt.Sprint(i)}})
		if err == nil {
			t.Fatal("refused connection succeeded")
		}
		assertBudgetEmpty(t, p)
	}
}

func TestByteBudgetGrantCancelledWhenLocalLimitWins(t *testing.T) {
	for _, kind := range []string{"count", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			p := fairPool(t, 8, 1)
			b := groupedBuffer(t, p, "tenant-a")
			if !b.Add(flaggedTestMessage(), time.Second) {
				t.Fatal("initial add")
			}
			p.reserveFor(6*testMsgSize, false, b.group, nil)
			_, _, _, w := p.reserveFor(testMsgSize, false, b.group, nil)
			p.releaseFor(testMsgSize, 0, b.group)
			select {
			case <-w.wake:
			default:
				t.Fatal("waiter not granted")
			}
			b.Lock()
			if kind == "count" {
				b.isAvailable.Clear()
			} else {
				b.maxBytes = testMsgSize
			}
			ok, next := b.tryAddLocked(flaggedTestMessage(), testMsgSize, w)
			b.Unlock()
			if ok || next != nil {
				t.Fatal("local limit retained closed grant")
			}
			if p.Used() != 6*testMsgSize {
				t.Fatal("local limit leaked grant")
			}
			p.releaseFor(5*testMsgSize, 0, b.group)
			b.Close()
			assertBudgetEmpty(t, p)
		})
	}
}

func BenchmarkFairByteBudgetReserveReleaseContended(b *testing.B) {
	p, err := NewFairByteBudget(1<<30, FairByteBudgetOptions{})
	if err != nil {
		b.Fatal(err)
	}
	var next atomic.Uint64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		g, err := p.register(fmt.Sprint(next.Add(1)))
		if err != nil {
			b.Fatal(err)
		}
		defer p.unregister(g)
		for pb.Next() {
			if ok, _, _, _ := p.reserveFor(1024, false, g, nil); !ok {
				b.Fatal("unexpected pressure")
			}
			p.releaseFor(1024, 0, g)
		}
	})
}

// One released byte rotates through1000 queued groups. Cancel/requeue the
// current owner so the next large head can retain that byte. This measures
// group-head indexes under saturation rather than broadcasting1000 wakeups.
func BenchmarkFairByteBudgetThousandWaitingGroups(b *testing.B) {
	const groups = 1000
	p, err := NewFairByteBudget(1<<20, FairByteBudgetOptions{})
	if err != nil {
		b.Fatal(err)
	}
	owner, _ := p.register("owner")
	p.reserveFor(p.total-p.reserve, false, owner, nil)
	gs := make([]*budgetGroup, groups)
	ws := make([]*budgetWaiter, groups)
	for i := range gs {
		gs[i], _ = p.register(fmt.Sprint(i))
		_, _, _, ws[i] = p.reserveFor(1<<20, false, gs[i], nil)
	}
	p.releaseFor(1, 0, owner)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx := i % groups
		p.cancel(ws[idx])
		_, _, _, ws[idx] = p.reserveFor(1<<20, false, gs[idx], nil)
	}
	b.StopTimer()
	for i, w := range ws {
		p.cancel(w)
		p.unregister(gs[i])
	}
	p.releaseFor(owner.used, 0, owner)
	p.unregister(owner)
}

func BenchmarkByteBudgetReserveReleaseContended(b *testing.B) {
	p := NewByteBudget(1 << 30)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if ok, _, _, _ := p.reserveFor(1024, false, nil, nil); !ok {
				b.Fatal("unexpected pressure")
			}
			p.release(1024)
		}
	})
}

func BenchmarkAckBufferFairBudgetContended(b *testing.B) {
	p, err := NewFairByteBudget(1<<40, FairByteBudgetOptions{})
	if err != nil {
		b.Fatal(err)
	}
	var next atomic.Uint64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		buf, err := NewAckBuffer(AckBufferOptions{SharedBudget: p, BudgetGroup: fmt.Sprint(next.Add(1))})
		if err != nil {
			b.Fatal(err)
		}
		defer buf.Close()
		buf.UpdateCapacity(1024)
		m := jsonMsg()
		for pb.Next() {
			if !buf.Add(m, time.Second) {
				b.Fatal("unexpected pressure")
			}
			n := buf.GetNextToDeliver(time.Second)
			if err := buf.Ack(n.SeqNum); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// The global one-frame exception cannot make a group with oversized escaped
// debt look empty when deciding who may enter the newcomer reserve.
func TestFairByteBudgetEscapedDebtCountsTowardOwnEligibility(t *testing.T) {
	p := fairPool(t, 64, 8)
	borrower := groupedBuffer(t, p, "borrower")
	for i := 0; i < 56; i++ {
		if !borrower.Add(flaggedTestMessage(), time.Second) {
			t.Fatal(i)
		}
	}
	a := groupedBuffer(t, p, "escaped")
	if !a.Add(textMsg(100*testMsgSize), time.Second) {
		t.Fatal("escape rejected")
	}
	if a.Add(flaggedTestMessage(), 10*time.Millisecond) {
		t.Fatal("oversized escaped group consumed newcomer reserve")
	}
	b := groupedBuffer(t, p, "healthy")
	for i := 0; i < 8; i++ {
		if !b.Add(flaggedTestMessage(), time.Second) {
			t.Fatalf("healthy%d", i)
		}
	}
}
