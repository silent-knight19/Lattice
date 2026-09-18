# Lattice: Production `pprof` Profiling & Diagnostics Runbook

* **Phase**: P13-S01-M04
* **Target Audience**: Systems Engineers, Performance Engineers, SREs, and Operators
* **Status**: Production Reference
* **Applicability**: Lattice Database Daemon (`cmd/lattice`)

---

## 1. Executive Summary & Architecture

Lattice provides built-in runtime profiling capabilities powered by the Go standard library's `runtime/pprof` subsystem. Profiling is exposed through a **dedicated, isolated HTTP diagnostics server** that runs alongside the primary database engine.

### Architectural Invariants
1. **Strict Transport Isolation**: The pprof HTTP server operates on an independent TCP listener. It never multiplexes HTTP over the database binary TCP transport port (default `9099`).
2. **Dedicated ServeMux**: Endpoints are registered on a dedicated `http.NewServeMux()` instance. Lattice never utilizes `http.DefaultServeMux`, preventing accidental handler leakage or third-party package pollution.
3. **Strict Loopback-Only Policy**: For security, the pprof HTTP server **fails closed** if configured to bind to a wildcard (`0.0.0.0`, `::`) or a non-loopback network interface (LAN or public IP). It accepts only `127.0.0.1`, `localhost`, `[::1]`, or loopback subnets.
4. **Opt-In by Default**: Profiling is disabled by default (`PprofAddress = ""`). Zero network ports or HTTP listeners are opened unless explicitly configured.
5. **Deterministic Lifecycle**: The pprof diagnostics server starts before the database accepts client traffic, drains active requests before closing during shutdown, and shuts down *before* the underlying LSM storage engine is closed and flushed.

---

## 2. Configuration & Startup

### Command-Line Flags
Enable pprof diagnostics by passing the `--pprof-address` flag to `lattice`:

```bash
# Start Lattice with pprof listening on local port 6060
./bin/lattice --data-dir ./data --port 9099 --pprof-address 127.0.0.1:6060
```

To let the operating system assign an ephemeral port (useful in integration test environments):

```bash
./bin/lattice --data-dir ./data --port 9099 --pprof-address 127.0.0.1:0
```

### Configuration File Options
In a JSON configuration file (`lattice.json`):

```json
{
  "data_dir": "/var/lib/lattice/data",
  "port": 9099,
  "pprof_address": "127.0.0.1:6060"
}
```

In a key-value or YAML configuration file (`lattice.conf`):

```ini
data_dir = /var/lib/lattice/data
port = 9099
pprof_address = 127.0.0.1:6060
```

### Startup Log Announcement
When enabled, the server announces the diagnostics endpoint on stdout during startup:

```text
lattice: pprof diagnostics listening on http://127.0.0.1:6060/debug/pprof/
lattice: server listening on 127.0.0.1:9099 (data-dir: /var/lib/lattice/data)
```

---

## 3. Available Diagnostics Endpoints

All diagnostic endpoints are hosted under `/debug/pprof/`. Accessing `/` or `/debug/pprof` automatically redirects to `/debug/pprof/`.

| Endpoint | Format | Description | Operational Use Case |
| :--- | :--- | :--- | :--- |
| `/debug/pprof/` | HTML | Interactive index linking all profiles | Quick browser inspection |
| `/debug/pprof/profile` | Proto (gzip) | CPU execution profile (default 30s) | Identifying CPU hotspots, lock spinning, serialization bottlenecks |
| `/debug/pprof/heap` | Proto (gzip) | Memory allocations and heap residency | Finding memory leaks, large object allocations, GC pressure |
| `/debug/pprof/goroutine` | Text / Proto | Stack traces of all live goroutines | Detecting goroutine leaks, stuck connections, deadlocks |
| `/debug/pprof/allocs` | Proto (gzip) | Cumulative past memory allocations | Optimizing garbage collection overhead and allocation rate |
| `/debug/pprof/block` | Proto (gzip) | Stack traces that led to blocking on sync primitives | Analyzing lock contention and channel blocking |
| `/debug/pprof/mutex` | Proto (gzip) | Holders of contended mutexes | Diagnosing mutex lock bottlenecks |
| `/debug/pprof/threadcreate` | Proto (gzip) | Stack traces that led to the creation of new OS threads | Investigating thread starvation or excessive cgo/syscall threads |
| `/debug/pprof/trace` | Binary | Execution trace over a sampling window | Microsecond-level visualization of scheduler, GC, syscall events |
| `/debug/pprof/cmdline` | Text | Command line arguments of the running process | Verifying running binary arguments |
| `/debug/pprof/symbol` | Text | Symbol lookup table | Resolving program counters to function names |

---

## 4. Hands-On Operational Workflows

### 4.1. CPU Profiling Under Benchmark Load

To analyze CPU consumption during high-throughput workloads (such as running `lattice-bench`):

1. **Start the benchmark workload**:
   ```bash
   ./bin/lattice-bench --address 127.0.0.1:9099 --concurrency 64 --duration 60s --read-ratio 0.5
   ```

2. **Collect a 30-second CPU profile**:
   ```bash
   go tool pprof -http=:8080 http://127.0.0.1:6060/debug/pprof/profile?seconds=30
   ```
   This captures stack samples for 30 seconds and launches an interactive web UI at `http://localhost:8080` displaying flame graphs, top functions, and call graphs.

3. **CLI Analysis Alternative**:
   ```bash
   # Download profile directly
   curl -s -o cpu.pb.gz "http://127.0.0.1:6060/debug/pprof/profile?seconds=30"
   
   # Inspect top 10 CPU consumers in terminal
   go tool pprof -top -cum cpu.pb.gz | head -n 25
   ```

### 4.2. Heap Profiling & Memory Leak Detection

Lattice tracks both *currently active resident memory* and *cumulative allocations since process start*.

1. **Inspect Live In-Use Memory** (identifies what is currently taking up RAM):
   ```bash
   go tool pprof -inuse_space http://127.0.0.1:6060/debug/pprof/heap
   ```
   Inside the pprof interactive prompt:
   ```text
   (pprof) top20
   (pprof) list MemTable
   ```

2. **Inspect Cumulative Allocations** (identifies what creates GC pressure):
   ```bash
   go tool pprof -alloc_space http://127.0.0.1:6060/debug/pprof/heap
   ```

3. **Compare Memory Growth Over Time (Differential Profiling)**:
   ```bash
   # Capture baseline profile at start of workload
   curl -s -o heap_base.pb.gz http://127.0.0.1:6060/debug/pprof/heap
   
   # ... run benchmark or wait for memory spike ...
   
   # Capture post-load profile
   curl -s -o heap_after.pb.gz http://127.0.0.1:6060/debug/pprof/heap
   
   # Generate differential profile
   go tool pprof -base heap_base.pb.gz -http=:8081 heap_after.pb.gz
   ```

### 4.3. Goroutine Leak & Deadlock Investigation

If the daemon appears unresponsive, connection counts spike, or client requests time out:

1. **Check Live Goroutine Count**:
   ```bash
   curl -s http://127.0.0.1:6060/debug/pprof/goroutine\?debug=1 | head -n 5
   ```

2. **Dump Full Goroutine Stack Traces**:
   ```bash
   curl -s http://127.0.0.1:6060/debug/pprof/goroutine\?debug=2 > goroutines.txt
   ```
   Inspect `goroutines.txt` for clusters of goroutines blocked on network reads, channel operations, or mutexes.

3. **Interactive Goroutine Analysis**:
   ```bash
   go tool pprof http://127.0.0.1:6060/debug/pprof/goroutine
   (pprof) top
   (pprof) traces
   ```

### 4.4. Execution Tracing (`go tool trace`)

For diagnosing microsecond-level latency spikes, GC pause pauses, or goroutine scheduling delays:

1. **Record a 5-Second Runtime Trace**:
   ```bash
   curl -s -o trace.out "http://127.0.0.1:6060/debug/pprof/trace?seconds=5"
   ```

2. **View the Trace in Browser**:
   ```bash
   go tool trace trace.out
   ```
   *Note: Chrome/Chromium is required to view the interactive timeline.*

### 4.5. Remote Server Profiling via SSH Tunnel

Because Lattice strictly forbids binding `pprof` to public or external network interfaces, remote profiling on production or staging instances must be conducted via a secure SSH tunnel:

1. **Establish a Local Port-Forwarding Tunnel**:
   ```bash
   # Forwards local port 6060 to remote host's 127.0.0.1:6060 through encrypted SSH
   ssh -N -L 6060:127.0.0.1:6060 user@remote-lattice-node.internal
   ```

2. **Run Local Profiling Tools Directly Against Forwarded Port**:
   ```bash
   go tool pprof -http=:8080 http://127.0.0.1:6060/debug/pprof/profile?seconds=30
   ```

3. **Close the Tunnel**:
   Terminate the SSH process (`Ctrl+C`). No ports remain open on the remote host's public interfaces.

---

## 5. Security & Threat Modeling

### Why Pprof Must Never Be Publicly Exposed
1. **Memory Content Exposure**: Heap profiles and core dumps can contain fragments of user keys, values, or credentials residing in memory buffers.
2. **Denial of Service (DoS)**: CPU profiling and execution tracing require kernel signals and runtime instrumentation that introduce 1-3% CPU overhead. A malicious actor repeatedly triggering `/debug/pprof/trace?seconds=300` could cause significant performance degradation.
3. **Internal Architecture Reconnaissance**: The `cmdline`, `symbol`, and `goroutine` endpoints expose exact runtime versions, command-line arguments, filesystem paths, and internal concurrency state.

### Defensive Safeguards in Lattice
* **Enforced Loopback Verification**: `Config.Validate()` and `NewPprofServer()` independently verify that the target address is a local loopback interface (`isLoopback(host)`).
* **Fail-Closed on Wildcards**: Attempting to bind `0.0.0.0` or `::` produces an immediate startup configuration error.
* **Immunity to `--insecure-transport`**: The `--insecure-transport` flag (which allows unencrypted TCP data plane communication) **explicitly does NOT apply to pprof**. Pprof remains loopback-only under all circumstances.

---

## 6. Performance Overhead Guidelines

| Diagnostics Mode | CPU Overhead | Memory Overhead | Safety in Production |
| :--- | :--- | :--- | :--- |
| **Idle (pprof enabled, no active queries)** | 0% | Negligible (< 1 MB for listener) | **100% Safe** (Continuous) |
| **Heap / Goroutine / Allocs Query** | Brief spike (< 5ms) | Low | **Safe** (On-demand) |
| **CPU Profiling (`?seconds=30`)** | ~1% - 3% | Low | **Safe** (Targeted investigations) |
| **Execution Trace (`?seconds=5`)** | ~5% - 15% (event stream) | High disk write throughput | **Use with caution** (< 10s runs) |

> [!NOTE]
> **Evidence Classification & Benchmarking Note (SEC-P13-M04-003)**: The figures in the table above represent standard Go runtime profiling heuristics and industry operational guidelines (Tier 1 Design Targets). In accordance with the Lattice Evidence Classification System (`docs/implementation-plan.md` Section 2), empirical profiling overhead benchmarks captured on concrete hardware/OS environments will be recorded during production performance characterization.

