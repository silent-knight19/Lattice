package admin

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/silent-knight19/lattice/internal/metrics"
)

// This file implements SEC-8 (SSE resource governance) together with the core of FND-4
// (the bounded event bus), because the two share one contract: a slow or absent consumer
// must NEVER be able to block a producer. A Raft transition hook or a compaction worker
// runs on the engine's critical path; it must not stall because a browser tab is not
// reading its event stream.
//
// The two-part rule:
//   - Publish NEVER blocks. A subscriber whose buffer is full has the event dropped and a
//     counter incremented.
//   - Subscriptions are bounded and refused BEFORE a channel is allocated, so a flood of
//     connections cannot exhaust memory.

// Event kinds published on the bus.
const (
	EventRaftTransition    = "raft.transition"
	EventCompactionStart   = "compaction.start"
	EventCompactionFinish  = "compaction.finish"
	EventCompactionFailure = "compaction.failure"
	EventFlush             = "flush"
	EventWALRotation       = "wal.rotation"
	EventRecoveryStart     = "recovery.start"
	EventRecoveryFinish    = "recovery.finish"
	EventOrphanCleanup     = "orphan.cleanup"
)

// maxEventFieldLen bounds every string carried in an event.
//
// SEC-8.6: event payloads are pushed straight into a browser. An unbounded string (a long
// key, a verbose error) would bloat every subscriber's buffer and could carry content that
// has no business being broadcast to every connected console.
const maxEventFieldLen = 256

// sanitizeEventField makes a string safe to place in an event payload.
//
// It strips control characters (blocking log/terminal escape injection in the rendering
// console and JSON smuggling) and truncates to a bounded length. Printable UTF-8 is left
// intact so the console can display it; invalid UTF-8 is replaced with U+FFFD.
func sanitizeEventField(s string) string {
	if len(s) > maxEventFieldLen {
		s = s[:maxEventFieldLen]
		// Truncating mid-rune would produce invalid UTF-8; cut back to a rune boundary.
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	var b []byte
	for _, r := range s {
		if r == utf8.RuneError {
			b = append(b, "�"...)
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		b = utf8.AppendRune(b, r)
	}
	return string(b)
}

// Event is a single item on the bus.
//
// Fields are strings and numbers only. There is deliberately no []byte field: raw key or
// value bytes in a broadcast payload would be an unbounded exfiltration channel to every
// connected console.
type Event struct {
	// Kind is the event type, e.g. raft.transition.
	Kind string `json:"kind"`
	// Time is when the event was created.
	Time time.Time `json:"time"`
	// NodeID is the emitting node, 0 when not applicable.
	NodeID uint64 `json:"node_id"`
	// Fields are sanitized on construction. Every value is a bounded, control-character-free
	// string: there is deliberately no []byte field, which would otherwise be an unbounded
	// exfiltration channel to every connected console.
	Fields map[string]string `json:"fields,omitempty"`
}

// NewEvent builds a sanitized event.
func NewEvent(kind string, nodeID uint64, fields map[string]string) Event {
	safe := make(map[string]string, len(fields))
	for k, v := range fields {
		safe[sanitizeEventField(k)] = sanitizeEventField(v)
	}
	return Event{Kind: sanitizeEventField(kind), Time: time.Now(), NodeID: nodeID, Fields: safe}
}

// ErrSubscriberLimit reports that the bus refused a subscription because it is at capacity.
var ErrSubscriberLimit = fmt.Errorf("admin: event subscriber limit reached")

// Subscription is one consumer's view of the bus.
type Subscription struct {
	id     uint64
	ch     chan Event
	bus    *EventBus
	closed atomic.Bool
}

// C exposes the event channel.
func (s *Subscription) C() <-chan Event {
	if s == nil {
		return nil
	}
	return s.ch
}

// Close unsubscribes and releases the slot. It is safe to call more than once.
func (s *Subscription) Close() {
	if s == nil || s.bus == nil {
		return
	}
	if s.closed.CompareAndSwap(false, true) {
		s.bus.remove(s.id)
	}
}

// subscriberBuffer is the per-subscriber queue depth.
//
// Small on purpose: the console only needs recent state (role, term, compaction status).
// A deep queue would let one stalled client accumulate unbounded memory, which is exactly
// the failure this design avoids.
const subscriberBuffer = 16

// EventBus is a bounded, non-blocking publish/subscribe hub.
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[uint64]*Subscription
	nextID      uint64

	recent    []Event
	recentCap int

	maxSubscribers int
	dropped        *metrics.Counter

	// notify wakes metric samplers so they coalesce instead of busy-looping.
	notify chan struct{}
}

// BusOptions configures an EventBus.
type BusOptions struct {
	// MaxSubscribers is the hard cap on concurrent subscriptions. 0 selects a default.
	MaxSubscribers int
	// RecentCapacity is the ring size backing Recent(). 0 selects a default.
	RecentCapacity int
}

// NewEventBus constructs a bounded bus.
func NewEventBus(opts BusOptions) *EventBus {
	if opts.MaxSubscribers <= 0 {
		opts.MaxSubscribers = DefaultMaxSubscribers
	}
	if opts.RecentCapacity <= 0 {
		opts.RecentCapacity = 256
	}
	return &EventBus{
		subscribers:    make(map[uint64]*Subscription),
		recent:         make([]Event, 0, opts.RecentCapacity),
		recentCap:      opts.RecentCapacity,
		maxSubscribers: opts.MaxSubscribers,
		dropped:        metrics.NewCounter(),
		notify:         make(chan struct{}, 1),
	}
}

// Subscribe registers a consumer.
//
// It fails with ErrSubscriberLimit when the bus is full, and it does so BEFORE allocating
// the channel, so a connection flood cannot be turned into a memory flood.
func (b *EventBus) Subscribe() (*Subscription, error) {
	if b == nil {
		return nil, ErrSubscriberLimit
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subscribers) >= b.maxSubscribers {
		// Refuse the NEWEST subscriber: existing clients keep working, which is the
		// least disruptive choice for an operator with the console already open.
		b.dropped.Inc()
		return nil, ErrSubscriberLimit
	}
	b.nextID++
	s := &Subscription{
		id:  b.nextID,
		ch:  make(chan Event, subscriberBuffer),
		bus: b,
	}
	b.subscribers[s.id] = s
	return s, nil
}

func (b *EventBus) remove(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subscribers, id)
}

// Publish delivers an event to every subscriber without blocking.
//
// If a subscriber's buffer is full its copy is dropped and the drop counter is incremented.
// The alternative — an unbounded queue or a blocking send — would let a single stalled
// browser tab stall the Raft hook that called Publish.
func (b *EventBus) Publish(ev Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	// Ring buffer for the Overview feed: fixed capacity, oldest evicted.
	if len(b.recent) >= b.recentCap {
		copy(b.recent, b.recent[1:])
		b.recent[len(b.recent)-1] = ev
	} else {
		b.recent = append(b.recent, ev)
	}
	subs := make([]*Subscription, 0, len(b.subscribers))
	for _, s := range b.subscribers {
		subs = append(subs, s)
	}
	b.mu.Unlock()

	for _, s := range subs {
		select {
		case s.ch <- ev:
		default:
			// Consumer is behind. Drop and count; never block the producer.
			b.dropped.Inc()
		}
	}
	b.signal()
}

// signal wakes a waiting sampler without blocking.
func (b *EventBus) signal() {
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

// Notify returns a channel signalled when events are published. Used by the metric
// sampler so it can coalesce to at most one wake-up per tick.
func (b *EventBus) Notify() <-chan struct{} {
	if b == nil {
		return nil
	}
	return b.notify
}

// Recent returns up to n of the most recent events, oldest first.
func (b *EventBus) Recent(n int) []Event {
	if b == nil || n <= 0 {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if n > len(b.recent) {
		n = len(b.recent)
	}
	out := make([]Event, n)
	copy(out, b.recent[len(b.recent)-n:])
	return out
}

// Subscribers reports the current subscriber count.
func (b *EventBus) Subscribers() int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// Dropped reports how many event deliveries were dropped because a consumer was behind.
func (b *EventBus) Dropped() uint64 {
	if b == nil {
		return 0
	}
	return b.dropped.Value()
}

// DroppedCounter exposes the underlying counter so the daemon can register it with the
// metrics registry as lattice_admin_events_dropped_total.
func (b *EventBus) DroppedCounter() *metrics.Counter {
	if b == nil {
		return nil
	}
	return b.dropped
}

// MaxSubscribers reports the configured cap.
func (b *EventBus) MaxSubscribers() int {
	if b == nil {
		return 0
	}
	return b.maxSubscribers
}
