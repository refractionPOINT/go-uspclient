package uspclient

import (
	"sync"
	"time"
)

type Event struct {
	c       *sync.Cond
	m       sync.Mutex
	v       bool
	waiters []chan struct{}
}

func NewEvent() *Event {
	e := &Event{}
	e.c = sync.NewCond(&e.m)
	return e
}

func (e *Event) Set() {
	e.m.Lock()
	defer e.m.Unlock()

	e.v = true
	e.c.Broadcast()

	if len(e.waiters) != 0 {
		for _, w := range e.waiters {
			w <- struct{}{}
			close(w)
		}
		e.waiters = nil
	}
}

func (e *Event) Clear() {
	e.m.Lock()
	defer e.m.Unlock()

	if !e.v {
		return
	}
	e.v = false
}

func (e *Event) Wait() {
	e.c.L.Lock()
	for !e.v {
		e.c.Wait()
	}
	e.c.L.Unlock()
}

func (e *Event) WaitFor(d time.Duration) bool {
	e.m.Lock()
	if e.v {
		e.m.Unlock()
		return true
	}

	w := make(chan struct{}, 1)
	e.waiters = append(e.waiters, w)
	e.m.Unlock()

	timeout := make(chan bool, 1)
	go func() {
		time.Sleep(d)
		timeout <- true
		close(timeout)
		e.m.Lock()
		for i, t := range e.waiters {
			if t == w {
				e.waiters = removeWaiter(e.waiters, i)
				break
			}
		}
		e.m.Unlock()
	}()

	select {
	case <-w:
		return true
	case <-timeout:
		return false
	}
}

func removeWaiter(s []chan struct{}, i int) []chan struct{} {
	s[len(s)-1], s[i] = s[i], s[len(s)-1]
	return s[:len(s)-1]
}

func (e *Event) IsSet() bool {
	return e.v
}
