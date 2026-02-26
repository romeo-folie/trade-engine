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
	C      <-chan Event // read-only channel for the subscriber
	ch     chan Event   // internal writeable reference
	evType Type
	bus    *Bus
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
		C:      ch,
		ch:     ch,
		evType: evType,
		bus:    b,
	}
	b.mu.Lock()
	b.subs[evType] = append(b.subs[evType], sub)
	b.mu.Unlock()
	return sub
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

	subs := b.subs[sub.evType]
	for i, s := range subs {
		if s == sub {
			b.subs[sub.evType] = append(subs[:i], subs[i+1:]...)
			close(sub.ch)
			return
		}
	}
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
		select {
		case sub.ch <- ev:
		default:
			// subscriber buffer full — drop event (non-blocking)
		}
	}

	// Deliver to wildcard subscribers.
	if evType != "*" {
		for _, sub := range b.subs["*"] {
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
