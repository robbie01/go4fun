package broadcast

import "sync"

type Set[T any] struct {
	lock sync.RWMutex
	set  map[chan<- T]struct{}
}

func (cs *Set[T]) MakeChan() Chan[T] {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	if cs.set == nil {
		cs.set = make(map[chan<- T]struct{})
	}

	ch := make(chan T)
	cs.set[ch] = struct{}{}
	return Chan[T]{
		inner: ch,
		cs:    cs,
	}
}

func (cs *Set[T]) Send(val T) {
	cs.lock.RLock()
	defer cs.lock.RUnlock()

	for ch := range cs.set {
		ch <- val
	}
}

func (cs *Set[T]) CloseAll() {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	for ch := range cs.set {
		close(ch)
	}

	clear(cs.set)
}

func (cs *Set[T]) remove(ch chan<- T) {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	delete(cs.set, ch)
}

type Chan[T any] struct {
	inner chan T
	cs    *Set[T]
}

func (ch *Chan[T]) Receiver() <-chan T {
	return ch.inner
}

func (ch *Chan[T]) Close() {
	ch.cs.remove(ch.inner)
	// By removing the channel from the set, we have effectively taken over the send half
	close(ch.inner)
}
