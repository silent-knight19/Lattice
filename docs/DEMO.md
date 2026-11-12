# Lattice Demo Walkthrough (CLI + Web Admin Console)

This guide walks through running Lattice locally, inspecting live storage and runtime telemetry in the embedded Web Admin Console, and exercising real read/write, compaction, and crash-recovery workflows.

---

## 1. Quickstart (60 Seconds)

Build the binaries and start a single-node daemon with the Admin Console enabled on loopback:

```bash
go build -o bin/lattice ./cmd/lattice
go build -o bin/lattice-cli ./cmd/lattice-cli
go build -o bin/lattice-bench ./cmd/lattice-bench

./bin/lattice --data-dir=./data --port=9099 --admin-address=127.0.0.1:7070
```

Open **`http://127.0.0.1:7070`** in your browser.

---

## 2. Demo Ladder

### Step 1: Overview & Real Host Telemetry
1. Open the **Overview** tab.
2. Observe the live daemon status banner, node uptime, MemTable fill bytes, WAL active segment bytes, and disk space.
3. Review the **Host System & Runtime Telemetry** panel showing live OS/architecture, CPU cores, active goroutines, heap/OS memory, and GC cycle count.

### Step 2: Write & Inspect Data via CLI or Browser Console REPL
In a terminal, use `lattice-cli`:
```bash
./bin/lattice-cli --address=127.0.0.1:9099
```
```text
lattice> PUT user:1001 "Alice"
lattice> GET user:1001
lattice> STATS
```
Or open the **Console REPL** tab in the Web Console and run the same engine commands (`STATS`, `PUT key:1 hello`, `GET key:1`, `HELP`).

Browse and filter keys in the **Keyspace Browser** tab.

### Step 3: Flush MemTable & Trigger Compaction
1. Open **LSM Tree Explorer**.
2. Click **Flush MemTable** to freeze the active SkipList and flush it to an L0 SSTable file on disk (`POST /api/v1/lsm/flush`).
3. Click **Trigger Compaction** to run a leveled compaction pass (`POST /api/v1/lsm/compact`).

### Step 4: Inspect Physical Storage (SSTables & WAL)
1. Open **SSTable Inspector** and select any `.sst` file to inspect its 4KB block index offsets, sizes, and Bloom filter bit count / hash functions.
2. Open **WAL & Replay** to inspect active and sealed WAL segments and verify CRC integrity.

### Step 5: Load Generation & Crash Replay Diagnostics
1. Open **Lab & Chaos**.
2. Configure concurrency, target QPS, and read ratio, then click **Start Real Workload** to drive concurrent `Get`/`Put` operations directly into the engine while watching live throughput and latency sparklines update on **Overview** and **Engine Metrics**.
3. Click **Run Crash Replay Diagnostic** to inspect WAL recovery consistency, and **Prune Obsolete Files** to clean unreferenced SSTables from `VersionSet`.
