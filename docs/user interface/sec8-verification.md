# SEC-8 — Implementation & Verification Record

> Task: SEC-8 of [`ui-console-tasks.md`](./ui-console-tasks.md). **Status: COMPLETE + VERIFIED.**

---

## What was built

| File | Contents |
|---|---|
| `internal/admin/events.go` | `EventBus` (bounded, non-blocking), `Subscription`, `Event`, `NewEvent`, `sanitizeEventField` |
| `internal/admin/sse.go` | `sseStreamHandler`, `ipLimiter`, `sseConnLimiter`, budgets, heartbeat, retry |
| `internal/admin/events_test.go` | Publish-never-blocks, capacity, ring, sanitization |
| `internal/admin/sse_test.go` | Capacity, per-IP, disconnect, leak, cancel, method |

Package total: **310 cases**, `-race` clean, **zero third-party dependencies**.

This also delivers the **core of FND-4**: the plan kept the bus and the SSE endpoint as
separate tasks, but they share one contract — a slow consumer must never stall a producer —
so implementing them together avoids a duplicate buffering layer.

---

## The one invariant everything else serves

> **Publish NEVER blocks.**

`Publish` runs on the engine's critical path (a Raft `TransitionHook`, a compaction worker
completion). If it blocked on a browser tab that stopped reading, one abandoned console tab
would stall consensus. So a subscriber whose buffer is full **has the event dropped and a
counter incremented**.

Verified: **10,000 publishes with a stalled subscriber complete without blocking**, and drops
are counted rather than silently discarded.

The per-subscriber buffer is deliberately small (16). The console needs *recent* state — role,
term, compaction status — not history. A deep queue would let one stalled client accumulate
unbounded memory, which is the failure this design exists to prevent.

---

## Separate budgets (subtask 8.1)

| Budget | Default | Why it differs from the metrics server |
|---|---|---|
| Global subscribers | **8** | vs. 256 scraper connections in `internal/metrics/server.go:20` |
| Per IP | **4** | blunts one host's reconnect storm |

The two workloads are opposites. A Prometheus scrape is a brief connection that closes; an SSE
client holds a goroutine, a buffered channel, and a TCP connection for its whole session. A
handful of console tabs is normal; hundreds is an attack.

`sseConnLimiter` mirrors `connLimiterListener` from `internal/metrics/server.go:63-98`,
rejecting at `Accept()` so a flood never reaches per-connection allocation.

---

## Refusal happens BEFORE allocation (subtask 8.3)

The handler checks the global and per-IP budgets **before** calling `bus.Subscribe()`, because
`Subscribe()` is what allocates the channel. Checking afterwards would already have spent the
memory being protected. The `Subscribe()` capacity check is retained as the race-condition
backstop (two clients can pass the pre-check simultaneously).

When full, the **newest** subscriber is refused with `503` and `Retry-After`; existing clients
keep working, which is least disruptive for an operator who already has the console open.

---

## Live verification

| Test | Result |
|---|---|
| 20 concurrent streams, global cap 3 | **accepted=3, refused=17** |
| 6 concurrent streams from one IP, cap 2 | **accepted=2** |
| 8 sequential connect/disconnect cycles | slot reclaimed every round, subscribers back to 0 |
| 30 streams opened and closed | **goroutines 7 → 3, subscribers 0** |
| context cancellation | handler returned promptly, slot released |
| live client | `retry: 3000`, `: connected`, then live frames |

Live stream as actually received:

```
retry: 3000

: connected

event: raft.transition
data: {"kind":"raft.transition","time":"...","node_id":1,
       "fields":{"from":"follower","term":"4","to":"candidate"}}
```

---

## Payload hygiene (subtask 8.6)

`NewEvent` sanitizes **every** field at construction, not at render time:

- control characters stripped (blocks ANSI-escape and log injection in the console),
- truncated to 256 bytes, cut on a rune boundary to avoid invalid UTF-8,
- invalid UTF-8 replaced with U+FFFD; printable unicode preserved so the console can display it.

`Event` has **no `[]byte` field by design**. Raw key/value bytes in a broadcast payload would be
an unbounded exfiltration channel to every connected console.

---

## Two anti-spoofing decisions

**`X-Forwarded-For` is ignored** for per-IP limiting. It is client-supplied and trivially
spoofed, so trusting it would let an attacker bypass the cap by varying a header. The per-IP
identity comes from `r.RemoteAddr` only.

**Heartbeat every 15s.** Without it an intermediary may silently close a connection carrying no
bytes, and the console would stop updating with no error visible to anyone. `X-Accel-Buffering:
no` additionally asks proxies not to buffer, which would defeat the heartbeat trick.

---

## A leak "finding" that was the test's fault

`TestSSE_NoGoroutineLeak` initially reported **7 → 84 goroutines**. The authoritative invariant
(`bus.Subscribers() == 0`) did **not** fire, which showed the server was releasing everything.

The growth was the **test client**: Go's default `http.Transport` keeps a readLoop and writeLoop
per connection, so 30 streams add ~60 client-side goroutines. A correct server looked like it
leaked.

Fixed by disabling keep-alives in the test client so each stream's goroutines vanish with the
body, then restructuring the assertion to check the server-side invariant **first** and treat
the goroutine count as a secondary signal.

Worth recording: had I trusted the goroutine number, I would have "fixed" a non-existent server
bug and probably introduced a real one.

---

## Subtask acceptance

| Subtask | Status |
|---|---|
| 8.1 Separate connection budget, enforced pre-allocation | ✅ cap 8, `sseConnLimiter` |
| 8.2 Per-IP cap | ✅ default 4 |
| 8.3 Subscriber cap, refuse newest, never block producer | ✅ verified |
| 8.4 `r.Context().Done()` frees all resources | ✅ leak + cancel tests |
| 8.5 Heartbeat + `retry:` | ✅ both verified live |
| 8.6 No secrets or unbounded strings in payloads | ✅ sanitization tests |
| 100 concurrent connects → at most the cap | ✅ 20- and 100-connect tests |
| no goroutine leak | ✅ 7 → 3 |
| `-race` clean | ✅ |

**SEC-8 acceptance criteria: met.**

---

## Remaining

- **SEC-6** is largely satisfied by `BindLoopback`; what is left is flag wiring and the startup
  warning (ADM-7).
- **SEC-9** completes `respond.go` with request IDs and error-code mapping.
- **FND-4**'s remaining scope — wiring producers (Raft hook, compaction, recovery) into the
  bus — happens with ADM-7, when the engine handles are available.
