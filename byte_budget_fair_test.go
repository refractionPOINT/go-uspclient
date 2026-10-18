package uspclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/google/uuid"
	"sync"
	"testing"
	"time"

	"github.com/refractionPOINT/go-uspclient/protocol"
)

func fairPool(t *testing.T, frames uint64, slots uint32) *ByteBudget {
	t.Helper()
	p, err := NewFairByteBudget(frames*testMsgSize, slots)
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

// A parks before B even registers. It consumes every byte it can borrow via
// four clients. B must retain a multi-frame window without any ACK from A.
func TestFairByteBudgetParkedTenantCannotConsumeNewTenantFloor(t *testing.T) {
	p := fairPool(t, 64, 2) // 16 protected frames per tenant, 32 lendable frames.
	var parked []*AckBuffer
	for i := 0; i < 4; i++ {
		parked = append(parked, groupedBuffer(t, p, "tenant-a"))
	}
	admitted := 0
	for ; admitted < 128; admitted++ {
		if !parked[admitted%len(parked)].Add(flaggedTestMessage(), 10*time.Millisecond) {
			break
		}
	}
	if admitted == 128 {
		t.Fatal("parked tenant has no memory bound")
	}
	healthy := groupedBuffer(t, p, "tenant-b")
	for round := 0; round < 20; round++ {
		for i := 0; i < 16; i++ {
			if !healthy.Add(flaggedTestMessage(), time.Second) {
				t.Fatalf("healthy tenant stalled at round=%d frame=%d", round, i)
			}
		}
		if got := p.Used(); got != 64*testMsgSize {
			t.Fatalf("used=%d want=%d", got, 64*testMsgSize)
		}
		deliverAndAck(t, healthy, 16)
	}
	if admitted != 48 {
		t.Fatalf("parked admitted=%d want48", admitted)
	}
	if got, _ := parked[0].GetUnAcked(); len(got) != 12 {
		t.Fatalf("parked messages changed: %d", len(got))
	}
}

// The existing per-client one-message escape remains charged to Used, but
// cannot consume another group's protected entitlement.
func TestFairByteBudgetEscapePreservesOtherTenantFloor(t *testing.T) {
	p := fairPool(t, 64, 2)
	a := groupedBuffer(t, p, "tenant-a")
	for i := 0; i < 48; i++ {
		if !a.Add(flaggedTestMessage(), time.Second) {
			t.Fatal(i)
		}
	}
	escape := groupedBuffer(t, p, "tenant-a")
	if !escape.Add(textMsg(100*testMsgSize), time.Second) {
		t.Fatal("oversized first message must escape")
	}
	healthy := groupedBuffer(t, p, "tenant-b")
	for i := 0; i < 16; i++ {
		if !healthy.Add(flaggedTestMessage(), time.Second) {
			t.Fatalf("healthy admission %d", i)
		}
	}
	escaped, _ := escape.GetUnAcked()
	if got := p.Used(); got != 64*testMsgSize+estimateMessageSize(escaped[0]) {
		t.Fatalf("escape accounting: %d", got)
	}
	deliverAndAck(t, escape, 1)
	escape.Close() // ACK followed by Close must not return the escape twice.
	if p.Used() != 64*testMsgSize {
		t.Fatal("escape release changed healthy bytes")
	}
}

func TestFairByteBudgetGroupRegistrationAndReuse(t *testing.T) {
	for _, tc := range []struct {
		total uint64
		slots uint32
	}{{0, 2}, {100, 0}, {3, 2}} {
		if _, err := NewFairByteBudget(tc.total, tc.slots); err == nil {
			t.Fatalf("accepted invalid size %+v", tc)
		}
	}
	p := fairPool(t, 64, 2)
	if _, err := NewAckBuffer(AckBufferOptions{SharedBudget: p}); err == nil {
		t.Fatal("missing group accepted")
	}
	a := groupedBuffer(t, p, "tenant-a")
	sibling := groupedBuffer(t, p, "tenant-a")
	b := groupedBuffer(t, p, "tenant-b")
	if _, err := NewAckBuffer(AckBufferOptions{SharedBudget: p, BudgetGroup: "tenant-c"}); err == nil {
		t.Fatal("capacity overflow accepted")
	}
	a.Close()
	if _, err := NewAckBuffer(AckBufferOptions{SharedBudget: p, BudgetGroup: "tenant-c"}); err == nil {
		t.Fatal("sibling still owns group")
	}
	sibling.Close()
	c := groupedBuffer(t, p, "tenant-c")
	if !c.Add(flaggedTestMessage(), time.Second) || !b.Add(flaggedTestMessage(), time.Second) {
		t.Fatal("reused slot cannot admit")
	}
	a.Close()
	b.Close()
	c.Close()
	sibling.Close()
	assertBudgetEmpty(t, p)
}

func assertBudgetEmpty(t *testing.T, p *ByteBudget) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used != 0 || p.escape != 0 || p.waiters.Len() != 0 || len(p.groups) != 0 || p.protected != p.floor*uint64(p.slots) {
		t.Fatalf("budget leaked: used=%d escape=%d waiters=%d groups=%d protected=%d", p.used, p.escape, p.waiters.Len(), len(p.groups), p.protected)
	}
}

// Waiter grants belong to an individual caller; a release too small for a
// waiter does not wake it, and a later caller cannot steal a granted byte.
func TestByteBudgetIndividualWaiterGrantsAndCancellation(t *testing.T) {
	p := NewByteBudget(10)
	if ok, _, _, _ := p.reserveFor(10, false, nil, nil); !ok {
		t.Fatal("initial reserve")
	}
	_, _, _, large := p.reserveFor(8, false, nil, nil)
	_, _, _, small := p.reserveFor(2, false, nil, nil)
	p.release(2)
	select {
	case <-large.wake:
		t.Fatal("unfit waiter was woken")
	default:
	}
	select {
	case <-small.wake:
	default:
		t.Fatal("eligible waiter not woken")
	}
	if p.Used() != 10 {
		t.Fatal("grant not reserved")
	}
	if ok, _, _, late := p.reserveFor(1, false, nil, nil); ok {
		t.Fatal("late producer stole grant")
	} else {
		p.cancel(late)
	}
	p.cancel(small) // Timed out after wake but before consuming the reservation.
	if p.Used() != 8 {
		t.Fatal("cancelled grant leaked")
	}
	p.release(6)
	select {
	case <-large.wake:
	default:
		t.Fatal("large waiter not woken")
	}
	ok, _, _, next := p.reserveFor(8, false, nil, large)
	if !ok || next != nil || p.Used() != 10 {
		t.Fatal("consuming grant charged twice")
	}
	p.release(10)
	assertBudgetEmpty(t, p)
}

// A group slot must remain owned until its last unconsumed waiter grant is
// cancelled, even if the buffer has already closed.
func TestFairByteBudgetPendingGrantKeepsGroupRegistered(t *testing.T) {
	p := fairPool(t, 8, 1)
	g, err := p.register("tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	p.reserveFor(8*testMsgSize, false, g, nil)
	_, _, _, w := p.reserveFor(testMsgSize, false, g, nil)
	p.releaseFor(testMsgSize, 0, g)
	p.releaseFor(7*testMsgSize, 0, g)
	p.unregister(g)
	if _, err := p.register("tenant-b"); err == nil {
		t.Fatal("pending waiter lost its group slot")
	}
	p.cancel(w)
	// The waiter was the group's last owner; no caller can touch g afterwards.
	b := groupedBuffer(t, p, "tenant-b")
	b.Close()
	assertBudgetEmpty(t, p)
}

// Concurrent ACKs, timeout/close races and repeated reuse of the same group
// exercise reservation ownership without relying on timing to observe a leak.
func TestFairByteBudgetConcurrentTenantLifecycle(t *testing.T) {
	p := fairPool(t, 32, 8)
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
		_, err := NewClient(context.Background(), ClientOptions{
			Identity:      Identity{Oid: uuid.NewString(), InstallationKey: "iid"},
			DestURL:       "ws" + strings.TrimPrefix(server.URL, "http") + "/usp",
			BufferOptions: AckBufferOptions{SharedBudget: p, BudgetGroup: fmt.Sprint(i)},
		})
		if err == nil {
			t.Fatal("refused connection succeeded")
		}
		assertBudgetEmpty(t, p)
	}
}

// A grant can arrive just before another Add consumes this buffer's last
// count/byte slot. Returning a closed grant channel here would busy-loop.
func TestByteBudgetGrantCancelledWhenLocalLimitWins(t *testing.T) {
	for _, kind := range []string{"count", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			p := fairPool(t, 8, 1)
			b := groupedBuffer(t, p, "tenant-a")
			m := flaggedTestMessage()
			if !b.Add(m, time.Second) {
				t.Fatal("initial add")
			}
			p.reserveFor(7*testMsgSize, false, b.group, nil)
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
				t.Fatal("local limit retained unusable grant")
			}
			if p.Used() != 7*testMsgSize {
				t.Fatal("local limit leaked grant")
			}
			p.releaseFor(6*testMsgSize, 0, b.group)
			b.Close()
			assertBudgetEmpty(t, p)
		})
	}
}
