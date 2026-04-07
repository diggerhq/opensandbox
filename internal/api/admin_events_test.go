package api

import (
	"sync"
	"testing"
	"time"
)

func TestPublishAndHistory(t *testing.T) {
	bus := NewAdminEventBus()

	bus.Publish("create", "sb-1", "w-1", "created sandbox")
	bus.Publish("destroy", "sb-2", "w-1", "destroyed sandbox")

	history := bus.History()
	if len(history) != 2 {
		t.Fatalf("expected 2 events in history, got %d", len(history))
	}
	if history[0].Type != "create" {
		t.Errorf("expected first event type 'create', got '%s'", history[0].Type)
	}
	if history[0].Sandbox != "sb-1" {
		t.Errorf("expected first event sandbox 'sb-1', got '%s'", history[0].Sandbox)
	}
	if history[1].Type != "destroy" {
		t.Errorf("expected second event type 'destroy', got '%s'", history[1].Type)
	}
}

func TestPublishEventFields(t *testing.T) {
	bus := NewAdminEventBus()

	bus.Publish("scale", "sb-99", "w-5", "scaled up")

	history := bus.History()
	if len(history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(history))
	}
	evt := history[0]
	if evt.Type != "scale" {
		t.Errorf("expected type 'scale', got '%s'", evt.Type)
	}
	if evt.Sandbox != "sb-99" {
		t.Errorf("expected sandbox 'sb-99', got '%s'", evt.Sandbox)
	}
	if evt.Worker != "w-5" {
		t.Errorf("expected worker 'w-5', got '%s'", evt.Worker)
	}
	if evt.Detail != "scaled up" {
		t.Errorf("expected detail 'scaled up', got '%s'", evt.Detail)
	}
	if evt.Time == "" {
		t.Error("expected non-empty time")
	}
}

func TestSubscribeReceivesEvents(t *testing.T) {
	bus := NewAdminEventBus()

	ch, cleanup := bus.subscribe()
	defer cleanup()

	bus.Publish("create", "sb-1", "w-1", "test")

	select {
	case evt := <-ch:
		if evt.Type != "create" {
			t.Errorf("expected event type 'create', got '%s'", evt.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event on subscriber channel")
	}
}

func TestSubscribeCleanup(t *testing.T) {
	bus := NewAdminEventBus()

	ch, cleanup := bus.subscribe()

	// Verify client was registered
	bus.mu.RLock()
	clientCount := len(bus.clients)
	bus.mu.RUnlock()
	if clientCount != 1 {
		t.Fatalf("expected 1 client, got %d", clientCount)
	}

	cleanup()

	// Verify client was removed
	bus.mu.RLock()
	clientCount = len(bus.clients)
	bus.mu.RUnlock()
	if clientCount != 0 {
		t.Fatalf("expected 0 clients after cleanup, got %d", clientCount)
	}

	// Channel should be closed
	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed after cleanup")
	}
}

func TestHistoryBufferCap(t *testing.T) {
	bus := NewAdminEventBus()

	// Publish more than the 2000-event cap
	for i := 0; i < 2050; i++ {
		bus.Publish("create", "sb", "w", "event")
	}

	history := bus.History()
	if len(history) != 2000 {
		t.Fatalf("expected history capped at 2000, got %d", len(history))
	}
}

func TestHistoryReturnsCopy(t *testing.T) {
	bus := NewAdminEventBus()

	bus.Publish("create", "sb-1", "w-1", "first")

	history1 := bus.History()
	history1[0].Type = "modified"

	history2 := bus.History()
	if history2[0].Type != "create" {
		t.Error("History() should return a copy; modifying the result should not affect internal state")
	}
}

func TestEmptyHistory(t *testing.T) {
	bus := NewAdminEventBus()

	history := bus.History()
	if len(history) != 0 {
		t.Fatalf("expected empty history, got %d events", len(history))
	}
}

func TestMultipleSubscribers(t *testing.T) {
	bus := NewAdminEventBus()

	ch1, cleanup1 := bus.subscribe()
	defer cleanup1()
	ch2, cleanup2 := bus.subscribe()
	defer cleanup2()

	bus.Publish("error", "sb-1", "w-1", "something broke")

	for i, ch := range []chan AdminEvent{ch1, ch2} {
		select {
		case evt := <-ch:
			if evt.Type != "error" {
				t.Errorf("subscriber %d: expected type 'error', got '%s'", i, evt.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timed out waiting for event", i)
		}
	}
}

func TestConcurrentPublish(t *testing.T) {
	bus := NewAdminEventBus()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			bus.Publish("create", "sb", "w", "concurrent")
		}(i)
	}
	wg.Wait()

	history := bus.History()
	if len(history) != 100 {
		t.Fatalf("expected 100 events after concurrent publish, got %d", len(history))
	}
}

func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	bus := NewAdminEventBus()

	// Subscribe but never read from the channel
	_, cleanup := bus.subscribe()
	defer cleanup()

	// Publish more events than the channel buffer (50)
	// This should not block thanks to non-blocking send
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			bus.Publish("create", "sb", "w", "flood")
		}
		close(done)
	}()

	select {
	case <-done:
		// Good, did not block
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on slow subscriber")
	}
}
