package admin

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestEventBus_PublishNeverBlocks is the central SEC-8 contract: a stalled consumer must not
// stall the producer. The producer here stands in for a Raft transition hook running on the
// engine's critical path.
func TestEventBus_PublishNeverBlocks(t *testing.T) {
	bus := NewEventBus(BusOptions{MaxSubscribers: 4})
	sub, err := bus.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	// The subscriber never reads. Publishing far more than the buffer must still be fast.
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		for i := 0; i < 10000; i++ {
			bus.Publish(NewEvent(EventCompactionFinish, 1, map[string]string{"i": "x"}))
		}
		done <- time.Since(start)
	}()

	select {
	case d := <-done:
		t.Logf("10000 publishes with a stalled subscriber took %v", d)
		if d > 5*time.Second {
			t.Errorf("Publish appears to block: %v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Publish blocked on a stalled subscriber")
	}

	// Every over-capacity delivery must be counted, not silently discarded.
	if bus.Dropped() == 0 {
		t.Error("expected dropped deliveries to be counted")
	}
}

// TestEventBus_SubscribeRefusesAtCapacity verifies the cap and that refusal happens before
// an allocation can be exploited.
func TestEventBus_SubscribeRefusesAtCapacity(t *testing.T) {
	bus := NewEventBus(BusOptions{MaxSubscribers: 3})
	var subs []*Subscription
	for i := 0; i < 3; i++ {
		s, err := bus.Subscribe()
		if err != nil {
			t.Fatalf("subscribe %d failed early: %v", i, err)
		}
		subs = append(subs, s)
	}
	if _, err := bus.Subscribe(); err == nil {
		t.Error("expected the 4th subscription to be refused at a cap of 3")
	}
	// Closing one frees a slot.
	subs[0].Close()
	s, err := bus.Subscribe()
	if err != nil {
		t.Errorf("subscribe after release failed: %v", err)
	} else {
		s.Close()
	}
	for _, sub := range subs[1:] {
		sub.Close()
	}
	if bus.Subscribers() != 0 {
		t.Errorf("Subscribers() = %d after closing all, want 0", bus.Subscribers())
	}
}

// TestEventBus_Subscribe100Concurrent asserts only the cap succeeds and no goroutine leaks.
func TestEventBus_Subscribe100Concurrent(t *testing.T) {
	bus := NewEventBus(BusOptions{MaxSubscribers: 8})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var accepted, refused int
	kept := make([]*Subscription, 0, 8)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := bus.Subscribe()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				refused++
				return
			}
			accepted++
			kept = append(kept, s)
		}()
	}
	wg.Wait()

	if accepted > bus.MaxSubscribers() {
		t.Errorf("accepted %d subscriptions with a cap of %d", accepted, bus.MaxSubscribers())
	}
	if accepted != 8 {
		t.Errorf("accepted = %d, want exactly the cap (8)", accepted)
	}
	if refused != 100-accepted {
		t.Errorf("refused = %d, want %d", refused, 100-accepted)
	}
	if bus.Subscribers() != accepted {
		t.Errorf("Subscribers() = %d, want %d", bus.Subscribers(), accepted)
	}

	// Every slot must be reclaimable.
	for _, s := range kept {
		s.Close()
	}
	if bus.Subscribers() != 0 {
		t.Errorf("slots not reclaimed: %d", bus.Subscribers())
	}
}

// TestSubscription_CloseIsIdempotent guards against double-close under the race detector.
func TestSubscription_CloseIsIdempotent(t *testing.T) {
	bus := NewEventBus(BusOptions{})
	sub, err := bus.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); sub.Close() }()
	}
	wg.Wait()
	if bus.Subscribers() != 0 {
		t.Errorf("Subscribers() = %d after concurrent Close, want 0", bus.Subscribers())
	}
}

// TestEventBus_RecentRing verifies the Overview feed ring evicts oldest-first.
func TestEventBus_RecentRing(t *testing.T) {
	bus := NewEventBus(BusOptions{RecentCapacity: 3})
	for _, k := range []string{"a", "b", "c", "d"} {
		bus.Publish(NewEvent(k, 0, nil))
	}
	got := bus.Recent(10)
	if len(got) != 3 {
		t.Fatalf("Recent returned %d events, want 3", len(got))
	}
	want := []string{"b", "c", "d"}
	for i, w := range want {
		if got[i].Kind != w {
			t.Errorf("Recent[%d].Kind = %q, want %q", i, got[i].Kind, w)
		}
	}
	if len(bus.Recent(0)) != 0 {
		t.Error("Recent(0) should be empty")
	}
}

// TestNewEvent_SanitizesFields covers SEC-8.6: payloads must carry no unbounded or
// control-character-bearing strings.
func TestNewEvent_SanitizesFields(t *testing.T) {
	hostile := strings.Repeat("A", 5000) + "\n\r\x1b[31mFAKE"
	ev := NewEvent(EventCompactionFailure, 7, map[string]string{
		"err":     hostile,
		"control": "a\x00b\x07c",
		"unicode": "héllo→世界",
	})

	for k, v := range ev.Fields {
		if len(v) > maxEventFieldLen+8 {
			t.Errorf("field %q length %d exceeds the cap", k, len(v))
		}
		for _, r := range v {
			if r < 0x20 && r != '\t' {
				t.Errorf("field %q contains control character %q", k, r)
			}
		}
	}
	if strings.Contains(ev.Fields["err"], "\x1b") {
		t.Error("ANSI escape survived sanitization")
	}
	// Printable unicode must survive intact so the console can display it.
	if !strings.Contains(ev.Fields["unicode"], "世界") {
		t.Errorf("unicode mangled: %q", ev.Fields["unicode"])
	}
}

// TestEventBus_NilSafe ensures the bus tolerates nil receivers so shutdown ordering cannot
// panic.
func TestEventBus_NilSafe(t *testing.T) {
	var bus *EventBus
	bus.Publish(NewEvent(EventFlush, 0, nil))
	if bus.Subscribers() != 0 || bus.Dropped() != 0 || bus.MaxSubscribers() != 0 {
		t.Error("nil bus reported non-zero state")
	}
	if bus.Recent(5) != nil {
		t.Error("nil bus Recent should be nil")
	}
	if bus.DroppedCounter() != nil {
		t.Error("nil bus DroppedCounter should be nil")
	}
	if _, err := bus.Subscribe(); err == nil {
		t.Error("nil bus Subscribe should fail closed")
	}
	var sub *Subscription
	sub.Close()
	if sub.C() != nil {
		t.Error("nil subscription C() should be nil")
	}
}
