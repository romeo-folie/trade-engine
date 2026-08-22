package events

import (
	"sync"
	"testing"
	"time"
)

func TestPublishDeliversToSubscriber(t *testing.T) {
	bus := New()
	sub := bus.Subscribe(SignalGenerated)

	bus.Publish(SignalGenerated, SignalGeneratedPayload{
		Ticker: "AAPL", Action: "BUY", Confidence: 0.85,
	})

	select {
	case ev := <-sub.C:
		if ev.Type != SignalGenerated {
			t.Fatalf("expected %s, got %s", SignalGenerated, ev.Type)
		}
		p, ok := ev.Payload.(SignalGeneratedPayload)
		if !ok {
			t.Fatal("payload type assertion failed")
		}
		if p.Ticker != "AAPL" || p.Confidence != 0.85 {
			t.Fatalf("unexpected payload: %+v", p)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestSubscriberDoesNotReceiveOtherTypes(t *testing.T) {
	bus := New()
	sub := bus.Subscribe(TradeExecuted)

	bus.Publish(SignalGenerated, SignalGeneratedPayload{Ticker: "TSLA"})

	select {
	case ev := <-sub.C:
		t.Fatalf("should not have received event: %+v", ev)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestSubscribeAll(t *testing.T) {
	bus := New()
	sub := bus.SubscribeAll()

	bus.Publish(SignalGenerated, SignalGeneratedPayload{Ticker: "A"})
	bus.Publish(TradeExecuted, TradeExecutedPayload{Ticker: "B"})
	bus.Publish(ErrorOccurred, ErrorPayload{Message: "test"})

	received := 0
	timeout := time.After(time.Second)
	for received < 3 {
		select {
		case <-sub.C:
			received++
		case <-timeout:
			t.Fatalf("expected 3 events, got %d", received)
		}
	}
}

func TestSubscribeLosslessRetainsLifecycleBurst(t *testing.T) {
	bus := New()
	sub := bus.SubscribeLossless(ScanStarted, ScannerItemAdded, ScanCompleted)
	defer bus.Unsubscribe(sub)

	const count = DefaultBufferSize + 64
	for i := 0; i < count; i++ {
		bus.Publish(ScannerItemAdded, ScannerItemAddedPayload{Ticker: "BURST"})
	}
	for i := 0; i < count; i++ {
		select {
		case ev := <-sub.C:
			if ev.Type != ScannerItemAdded {
				t.Fatalf("event %d = %s, want scanner item", i, ev.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out after receiving %d of %d events", i, count)
		}
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	bus := New()
	sub := bus.Subscribe(DataFetched)
	bus.Unsubscribe(sub)

	bus.Publish(DataFetched, DataFetchedPayload{Ticker: "GOOG"})

	// Channel should be closed.
	_, ok := <-sub.C
	if ok {
		t.Fatal("channel should be closed after unsubscribe")
	}
	if bus.SubscriberCount() != 0 {
		t.Fatalf("expected 0 subscribers, got %d", bus.SubscriberCount())
	}
}

func TestPublishDoesNotBlockWhenBufferFull(t *testing.T) {
	bus := New()
	_ = bus.SubscribeBuffered(DataFetched, 1) // buffer of 1

	// Publish 10 events — should not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			bus.Publish(DataFetched, DataFetchedPayload{Ticker: "X", Count: i})
		}
		close(done)
	}()

	select {
	case <-done:
		// expected: non-blocking
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on full buffer")
	}
}

func TestMultipleSubscribersFanOut(t *testing.T) {
	bus := New()
	sub1 := bus.Subscribe(AnalysisCompleted)
	sub2 := bus.Subscribe(AnalysisCompleted)

	bus.Publish(AnalysisCompleted, AnalysisCompletedPayload{Ticker: "MSFT"})

	for _, sub := range []*Subscription{sub1, sub2} {
		select {
		case ev := <-sub.C:
			p := ev.Payload.(AnalysisCompletedPayload)
			if p.Ticker != "MSFT" {
				t.Fatalf("expected MSFT, got %s", p.Ticker)
			}
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive event")
		}
	}
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	bus := New()
	var wg sync.WaitGroup

	// 5 subscribers.
	subs := make([]*Subscription, 5)
	for i := 0; i < 5; i++ {
		subs[i] = bus.Subscribe(SignalGenerated)
	}

	// 10 concurrent publishers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			bus.Publish(SignalGenerated, SignalGeneratedPayload{Ticker: "RACE"})
		}(i)
	}

	wg.Wait()

	// Each subscriber should have received up to 10 events.
	for i, sub := range subs {
		count := 0
		for {
			select {
			case <-sub.C:
				count++
			default:
				goto done
			}
		}
	done:
		if count == 0 {
			t.Fatalf("subscriber %d received 0 events", i)
		}
	}
}
