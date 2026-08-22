package events

import (
	"sync"
	"time"
)

const (
	// DefaultBufferSize is the per-subscriber channel buffer.
	// Large enough to absorb bursts without blocking the engine loop.
	DefaultBufferSize = 256
)

// Subscription is a handle returned by Subscribe. Call Unsubscribe to stop
// receiving events and release the channel.
type Subscription struct {
	C       <-chan Event // read-only channel for the subscriber
	ch      chan Event   // internal writeable reference
	evType  Type
	evTypes []Type
	bus     *Bus

	// A lossless subscription queues publishes under a short mutex, then a
	// dedicated dispatcher forwards them to C. It is used for lifecycle fences
	// where dropping a terminal event would strand earlier work indefinitely.
	lossless       bool
	pendingMu      sync.Mutex
	pending        []Event
	wake           chan struct{}
	done           chan struct{}
	dispatcherDone chan struct{}
}

// Bus is a fan-out publish-subscribe event bus. Thread-safe.
//
// Publishers call Publish() from any goroutine. Subscribers call Subscribe()
// to get a buffered channel of events. The bus never blocks the publisher —
// if a subscriber's buffer is full, that event is dropped for that subscriber.
type Bus struct {
	mu   sync.RWMutex
	subs map[Type][]*Subscription
}

// New creates a new event bus.
func New() *Bus {
	return &Bus{
		subs: make(map[Type][]*Subscription),
	}
}

// Subscribe registers a new subscriber for the given event type.
// Returns a Subscription whose C channel receives events.
func (b *Bus) Subscribe(evType Type) *Subscription {
	return b.SubscribeBuffered(evType, DefaultBufferSize)
}

// SubscribeBuffered registers a subscriber with a custom buffer size.
func (b *Bus) SubscribeBuffered(evType Type, bufferSize int) *Subscription {
	ch := make(chan Event, bufferSize)
	sub := &Subscription{
		C:       ch,
		ch:      ch,
		evType:  evType,
		evTypes: []Type{evType},
		bus:     b,
	}
	b.mu.Lock()
	b.subs[evType] = append(b.subs[evType], sub)
	b.mu.Unlock()
	return sub
}

// SubscribeLossless registers one subscription for the supplied event types.
// It keeps publisher calls non-blocking while retaining lifecycle events in an
// unbounded in-memory queue until the consumer accepts them. Callers should
// use it only for small, correctness-sensitive streams.
func (b *Bus) SubscribeLossless(evTypes ...Type) *Subscription {
	ch := make(chan Event, DefaultBufferSize)
	sub := &Subscription{
		C: ch, ch: ch, bus: b, evTypes: append([]Type(nil), evTypes...),
		lossless: true, wake: make(chan struct{}, 1), done: make(chan struct{}),
		dispatcherDone: make(chan struct{}),
	}
	b.mu.Lock()
	for _, evType := range sub.evTypes {
		b.subs[evType] = append(b.subs[evType], sub)
	}
	b.mu.Unlock()
	go sub.dispatchLossless()
	return sub
}

func (s *Subscription) dispatchLossless() {
	defer close(s.dispatcherDone)
	for {
		select {
		case <-s.done:
			return
		case <-s.wake:
		}
		for {
			s.pendingMu.Lock()
			if len(s.pending) == 0 {
				s.pendingMu.Unlock()
				break
			}
			ev := s.pending[0]
			s.pending[0] = Event{}
			s.pending = s.pending[1:]
			s.pendingMu.Unlock()
			select {
			case <-s.done:
				return
			case s.ch <- ev:
			}
		}
	}
}

func (s *Subscription) enqueueLossless(ev Event) {
	s.pendingMu.Lock()
	s.pending = append(s.pending, ev)
	s.pendingMu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// SubscribeAll registers a subscriber that receives ALL event types.
// Internally subscribes to a wildcard key.
func (b *Bus) SubscribeAll() *Subscription {
	return b.Subscribe("*")
}

// Unsubscribe removes a subscription and closes its channel.
func (b *Bus) Unsubscribe(sub *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, evType := range sub.evTypes {
		subs := b.subs[evType]
		for i, s := range subs {
			if s == sub {
				b.subs[evType] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
	}
	if sub.lossless {
		close(sub.done)
		<-sub.dispatcherDone
	}
	close(sub.ch)
}

// Publish sends an event to all subscribers of its type and all wildcard subscribers.
// Non-blocking: if a subscriber's buffer is full, the event is dropped for that subscriber.
func (b *Bus) Publish(evType Type, payload any) {
	ev := Event{
		Type:      evType,
		Timestamp: time.Now(),
		Payload:   payload,
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	// Deliver to type-specific subscribers.
	for _, sub := range b.subs[evType] {
		if sub.lossless {
			sub.enqueueLossless(ev)
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			// subscriber buffer full — drop event (non-blocking)
		}
	}

	// Deliver to wildcard subscribers.
	if evType != "*" {
		for _, sub := range b.subs["*"] {
			if sub.lossless {
				sub.enqueueLossless(ev)
				continue
			}
			select {
			case sub.ch <- ev:
			default:
			}
		}
	}
}

// SubscriberCount returns the total number of active subscriptions.
func (b *Bus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	count := 0
	for _, subs := range b.subs {
		count += len(subs)
	}
	return count
}
