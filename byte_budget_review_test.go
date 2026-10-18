package uspclient

import "testing"

// A forced escape returns partial credits and must immediately grant eligible
// waiters in other groups, without another release or unrelated admission.
func TestFairByteBudgetForcedEscapeReschedulesReleasedCredits(t *testing.T) {
	p, err := NewFairByteBudget(1000, FairByteBudgetOptions{MinGroupBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	filler, _ := p.register("filler")
	small, _ := p.register("small")
	x, _ := p.register("x")
	y, _ := p.register("y")
	if ok, _, _, _ := p.reserveFor(875, false, filler, nil); !ok {
		t.Fatal("base fill")
	}
	if ok, _, _, _ := p.reserveFor(125, false, small, nil); !ok {
		t.Fatal("reserve fill")
	}
	_, _, _, wx := p.reserveFor(100, false, x, nil)
	_, _, _, wy := p.reserveFor(50, false, y, nil)
	if wx == nil || wy == nil {
		t.Fatal("expected two waiters")
	}
	p.releaseFor(60, 0, small) // round robin: x head takes 60 partial credit
	if wx.credit != 60 || wy.credit != 0 {
		t.Fatalf("setup: x credit %d y credit %d", wx.credit, wy.credit)
	}
	// x's client no longer holds a flagged message: add() retries with force.
	ok, overshot, _, _ := p.reserveFor(100, true, x, wx)
	if !ok || !overshot {
		t.Fatalf("forced escape: ok=%v overshot=%v", ok, overshot)
	}
	p.mu.Lock()
	free := p.total - (p.used - p.escape)
	p.mu.Unlock()
	if free >= wy.n && !wy.granted {
		t.Fatalf("%d normal bytes free after the escape returned x's credits, but y's eligible %d-byte waiter was not granted (credit %d)", free, wy.n, wy.credit)
	}
	if !wy.granted {
		t.Fatal("eligible waiter was not granted on the escape")
	}
	select {
	case <-wy.wake:
	default:
		t.Fatal("granted waiter did not receive its individual wakeup")
	}
	p.cancel(wy)
	p.releaseFor(100, 100, x)
	p.releaseFor(875, 0, filler)
	p.releaseFor(65, 0, small)
	for _, g := range []*budgetGroup{filler, small, x, y} {
		p.unregister(g)
	}
	assertBudgetEmpty(t, p)
}
