package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/transport"
	"github.com/silent-knight19/lattice/internal/wal"
)

// AckRecord models an immutable acknowledged operation stored outside the daemon.
type AckRecord struct {
	OpID         string    `json:"op_id"`
	SeqNum       uint64    `json:"seq_num"`
	Key          string    `json:"key"`
	Value        string    `json:"value"`
	RequestID    uint64    `json:"request_id"`
	Generation   int64     `json:"generation"`
	Timestamp    time.Time `json:"timestamp"`
	Order        int       `json:"order"`
	ClientStatus string    `json:"client_status"`
}

// AckLedger is the parent-supervised, crash-independent persistence oracle for acknowledged writes.
// It resides outside the daemon data directory with 0700 dir and 0600 file permissions.
// Publication into memory occurs ONLY AFTER complete physical file write and Sync (Finding D, E).
type AckLedger struct {
	path    string
	mu      sync.RWMutex
	records []AckRecord
	byKey   map[string]AckRecord
	byOpID  map[string]AckRecord
	bySeq   map[uint64]AckRecord
	file    *os.File
	closed  atomic.Bool
	count   atomic.Int64
}

func newAckLedger(dir string) (*AckLedger, error) {
	// Finding 20: 0700 directory permissions
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create ack-ledger dir: %w", err)
	}
	// Finding 20: 0600 file permissions
	filePath := filepath.Join(dir, "ack_ledger.jsonl")
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open ack ledger file: %w", err)
	}
	return &AckLedger{
		path:   filePath,
		byKey:  make(map[string]AckRecord),
		byOpID: make(map[string]AckRecord),
		bySeq:  make(map[uint64]AckRecord),
		file:   f,
	}, nil
}

// RecordAck writes the record to disk, syncs it, and publishes it into memory indices.
// If disk write or sync fails, memory state is left 100% clean (Finding D).
// Duplicate OpID, SeqNum, or Key are strictly rejected (Finding E).
func (l *AckLedger) RecordAck(rec AckRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed.Load() {
		return errors.New("ack ledger is closed")
	}

	// 1. Validation
	if rec.OpID == "" || rec.Key == "" || rec.Value == "" || rec.SeqNum == 0 {
		return fmt.Errorf("invalid ack record: missing required fields")
	}

	// 2. Reject duplicates before disk write (Finding E)
	if _, exists := l.byKey[rec.Key]; exists {
		return fmt.Errorf("duplicate ack key in ledger: %s", rec.Key)
	}
	if _, exists := l.byOpID[rec.OpID]; exists {
		return fmt.Errorf("duplicate ack opID in ledger: %s", rec.OpID)
	}
	if _, exists := l.bySeq[rec.SeqNum]; exists {
		return fmt.Errorf("duplicate ack seqNum in ledger: %d", rec.SeqNum)
	}

	// 3. Serialization
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("failed to marshal ack record: %w", err)
	}
	data = append(data, '\n')

	// 4. Complete physical file write
	n, err := l.file.Write(data)
	if err != nil {
		return fmt.Errorf("ack ledger write failure: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("short write to ack ledger: %d != %d", n, len(data))
	}

	// 5. Durability sync barrier
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("ack ledger sync failure: %w", err)
	}

	// 6. Publication into memory ONLY after successful persistence (Finding D)
	l.records = append(l.records, rec)
	l.byKey[rec.Key] = rec
	l.byOpID[rec.OpID] = rec
	l.bySeq[rec.SeqNum] = rec
	l.count.Add(1)

	return nil
}

func (l *AckLedger) Snapshot() []AckRecord {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cp := make([]AckRecord, len(l.records))
	copy(cp, l.records)
	return cp
}

// ReadRecordsFromDisk bypasses volatile memory and reconstructs the oracle snapshot
// directly from the synced physical JSONL file on disk (FINDING-FID-01).
func (l *AckLedger) ReadRecordsFromDisk() ([]AckRecord, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	f, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to open ack ledger file for reading: %w", err)
	}
	defer f.Close()

	var records []AckRecord
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec AckRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("corrupt ack record on line %d: %w", lineNum, err)
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error reading ack ledger: %w", err)
	}
	return records, nil
}

func (l *AckLedger) Count() int {
	return int(l.count.Load())
}

func (l *AckLedger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed.Store(true)
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// safeStderrCollector collects up to maxBytes of stderr in a thread-safe manner (Finding F).
type safeStderrCollector struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func newSafeStderrCollector(maxBytes int) *safeStderrCollector {
	return &safeStderrCollector{max: maxBytes}
}

func (c *safeStderrCollector) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buf.Len() < c.max {
		remaining := c.max - c.buf.Len()
		if len(p) > remaining {
			c.buf.Write(p[:remaining])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (c *safeStderrCollector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// buildChildEnv constructs an allowlisted child environment to prevent ambient secret leakage (Finding G)
// and sandboxes HOME and TMPDIR inside testRoot to prevent ambient host pollution (FINDING-SEC-01).
func buildChildEnv(testRoot string) []string {
	allowlist := []string{
		"PATH", "SYSTEMROOT", "USER",
	}
	var env []string
	for _, k := range allowlist {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}

	homeDir := filepath.Join(testRoot, "home")
	_ = os.MkdirAll(homeDir, 0700)
	env = append(env, "HOME="+homeDir)

	tmpDir := filepath.Join(testRoot, "tmp")
	_ = os.MkdirAll(tmpDir, 0700)
	env = append(env, "TMPDIR="+tmpDir)

	return env
}

// DaemonProcess encapsulates a real compiled Lattice daemon subprocess supervised by the test.
type DaemonProcess struct {
	cmd        *exec.Cmd
	pid        int
	addr       string
	port       int
	dataDir    string
	binPath    string
	stderrColl *safeStderrCollector
	doneCh     chan error
	isDead     atomic.Bool
	mu         sync.Mutex
}

var serverListeningRegex = regexp.MustCompile(`lattice: server listening on (127\.0\.0\.1:\d+)`)

// startDaemonProcess spawns the real Lattice binary with --port 0, parses the actual bound address,
// and probes TCP readiness (Findings H, J, 21).
func startDaemonProcess(t *testing.T, binPath, dataDir string, gen int64) (*DaemonProcess, error) {
	// Finding J: bind port 0 to prevent TOCTOU port races and OS TIME_WAIT delays
	cmd := exec.Command(binPath, "--data-dir", dataDir, "--port", "0")
	// Finding G & FINDING-SEC-01: allowlisted environment with sandboxed HOME and TMPDIR
	testRoot := filepath.Dir(filepath.Clean(dataDir))
	cmd.Env = buildChildEnv(testRoot)
	// Finding 21: explicit working directory inside temporary test root
	cmd.Dir = filepath.Dir(binPath)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	// Finding F: safe, bounded stderr collector
	stderrColl := newSafeStderrCollector(64 * 1024)
	cmd.Stderr = stderrColl

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start daemon process: %w", err)
	}

	dp := &DaemonProcess{
		cmd:        cmd,
		pid:        cmd.Process.Pid,
		dataDir:    dataDir,
		binPath:    binPath,
		stderrColl: stderrColl,
		doneCh:     make(chan error, 1),
	}

	go func() {
		dp.doneCh <- cmd.Wait()
		dp.isDead.Store(true)
	}()

	addrCh := make(chan string, 1)
	var once sync.Once

	// SEC-P18-002: Continuously drain stdout to prevent child from blocking on full OS pipe buffer
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := scanner.Text()
			matches := serverListeningRegex.FindStringSubmatch(line)
			if len(matches) == 2 {
				once.Do(func() {
					addrCh <- matches[1]
				})
			}
		}
	}()

	var actualAddr string
	select {
	case actualAddr = <-addrCh:
		dp.addr = actualAddr
		_, portStr, err := net.SplitHostPort(actualAddr)
		if err == nil {
			dp.port, _ = strconv.Atoi(portStr)
		}
	case err := <-dp.doneCh:
		return nil, fmt.Errorf("daemon exited prematurely (err: %v, stderr: %s)", err, dp.stderrColl.String())
	case <-time.After(5 * time.Second):
		_ = dp.Kill()
		return nil, fmt.Errorf("timed out waiting for listening announcement (stderr: %s)", dp.stderrColl.String())
	}

	// Verify listener readiness via explicit TCP connection probe
	probeDeadline := time.Now().Add(3 * time.Second)
	var probeConn net.Conn
	var probeErr error
	for time.Now().Before(probeDeadline) {
		probeConn, probeErr = net.DialTimeout("tcp", dp.addr, 200*time.Millisecond)
		if probeErr == nil {
			_ = probeConn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if probeErr != nil {
		_ = dp.Kill()
		return nil, fmt.Errorf("daemon announced listening on %s but TCP probe failed: %w", dp.addr, probeErr)
	}

	return dp, nil
}

// Kill delivers an abrupt SIGKILL to the exact child process and verifies process exit (Finding H).
func (dp *DaemonProcess) Kill() error {
	dp.mu.Lock()
	defer dp.mu.Unlock()

	if dp.isDead.Load() {
		return nil
	}
	if dp.cmd == nil || dp.cmd.Process == nil {
		return nil
	}

	// os.Process.Kill sends SIGKILL on Unix-like systems
	_ = dp.cmd.Process.Kill()

	select {
	case <-dp.doneCh:
	case <-time.After(5 * time.Second):
		dp.isDead.Store(true)
		return fmt.Errorf("process %d failed to terminate within 5s after SIGKILL", dp.pid)
	}

	dp.isDead.Store(true)

	// Authoritative ProcessState check (Finding H): on Unix, SIGKILL sets ws.Signaled()
	if dp.cmd.ProcessState == nil {
		return fmt.Errorf("process %d state is nil", dp.pid)
	}
	if ws, ok := dp.cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if !ws.Exited() && !ws.Signaled() {
			return fmt.Errorf("process %d neither exited nor signaled", dp.pid)
		}
	}

	return nil
}

// IsDead asserts that the process has completely terminated at the OS level (Finding H).
func (dp *DaemonProcess) IsDead() bool {
	if dp.isDead.Load() {
		return true
	}
	p, err := os.FindProcess(dp.pid)
	if err != nil {
		return true
	}
	err = p.Signal(syscall.Signal(0))
	return err != nil
}

func (dp *DaemonProcess) PID() int {
	return dp.pid
}

func (dp *DaemonProcess) Addr() string {
	return dp.addr
}

// restartDaemonProcess restarts the daemon against the same persistent data directory.
func restartDaemonProcess(t *testing.T, binPath, dataDir string, gen int64) *DaemonProcess {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		dp, err := startDaemonProcess(t, binPath, dataDir, gen)
		if err == nil {
			return dp
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("failed to restart daemon generation %d within 10s: %v", gen, lastErr)
	return nil
}

// ContinuousWriter continuously issues Put operations over real TCP sockets.
// It implements true quiescence (Finding B), generation synchronization (Finding C),
// and fail-closed supervisor signaling without panicking (Finding A).
type ContinuousWriter struct {
	addrMu     sync.RWMutex
	addr       string
	runID      string
	ledger     *AckLedger
	genManager *atomic.Int64

	// Quiescence & State (Finding B)
	isPaused    atomic.Bool
	pauseMu     sync.Mutex
	pauseCond   *sync.Cond
	activeOps   sync.WaitGroup
	connMu      sync.Mutex
	activeConns map[net.Conn]struct{}

	// Fatal error propagation (Finding A)
	fatalErr   atomic.Pointer[error]
	fatalErrCh chan struct{}
	fatalOnce  sync.Once

	stopOnce        sync.Once
	stopCh          chan struct{}
	doneWg          sync.WaitGroup
	seqCounter      atomic.Uint64
	writesAttempted atomic.Uint64
	writesAcked     atomic.Uint64
	writesFailed    atomic.Uint64 // interrupted / broken connection / no-ack
	ackNotifyCh     chan struct{}
}

func newContinuousWriter(addr, runID string, ledger *AckLedger, genManager *atomic.Int64) *ContinuousWriter {
	cw := &ContinuousWriter{
		addr:        addr,
		runID:       runID,
		ledger:      ledger,
		genManager:  genManager,
		activeConns: make(map[net.Conn]struct{}),
		fatalErrCh:  make(chan struct{}),
		stopCh:      make(chan struct{}),
		ackNotifyCh: make(chan struct{}, 1024),
	}
	cw.pauseCond = sync.NewCond(&cw.pauseMu)
	return cw
}

func (w *ContinuousWriter) SetAddr(newAddr string) {
	w.addrMu.Lock()
	defer w.addrMu.Unlock()
	w.addr = newAddr
}

func (w *ContinuousWriter) getAddr() string {
	w.addrMu.RLock()
	defer w.addrMu.RUnlock()
	return w.addr
}

func (w *ContinuousWriter) Start(workers int) {
	for i := 0; i < workers; i++ {
		w.doneWg.Add(1)
		go w.workerLoop()
	}
}

func (w *ContinuousWriter) reportFatalError(err error) {
	w.fatalOnce.Do(func() {
		w.fatalErr.Store(&err)
		close(w.fatalErrCh)
	})
	// Trigger pause so workers cease attempting operations
	w.pauseMu.Lock()
	w.isPaused.Store(true)
	w.pauseMu.Unlock()
}

func (w *ContinuousWriter) FatalError() error {
	p := w.fatalErr.Load()
	if p == nil {
		return nil
	}
	return *p
}

func (w *ContinuousWriter) workerLoop() {
	defer w.doneWg.Done()

	for {
		select {
		case <-w.stopCh:
			return
		default:
		}

		if w.FatalError() != nil {
			return
		}

		// Quiescence barrier: park if paused
		w.pauseMu.Lock()
		for w.isPaused.Load() {
			select {
			case <-w.stopCh:
				w.pauseMu.Unlock()
				return
			default:
			}
			w.pauseCond.Wait()
		}
		// Redundant stopCh drainage on worker unpause to eliminate stale wakeups (FINDING-CONC-01)
		select {
		case <-w.stopCh:
			w.pauseMu.Unlock()
			return
		default:
		}
		// Register active operation before unlocking pauseMu to eliminate race window (Finding B)
		w.activeOps.Add(1)
		w.pauseMu.Unlock()

		w.executeSingleWrite()
		w.activeOps.Done()
	}
}

func (w *ContinuousWriter) registerConn(c net.Conn) {
	w.connMu.Lock()
	defer w.connMu.Unlock()
	w.activeConns[c] = struct{}{}
}

func (w *ContinuousWriter) unregisterConn(c net.Conn) {
	w.connMu.Lock()
	defer w.connMu.Unlock()
	delete(w.activeConns, c)
}

func (w *ContinuousWriter) executeSingleWrite() {
	if w.isPaused.Load() || w.FatalError() != nil {
		return
	}

	targetAddr := w.getAddr()
	conn, err := net.DialTimeout("tcp", targetAddr, 200*time.Millisecond)
	if err != nil {
		w.writesFailed.Add(1)
		time.Sleep(10 * time.Millisecond)
		return
	}
	w.registerConn(conn)
	defer func() {
		w.unregisterConn(conn)
		_ = conn.Close()
	}()

	seq := w.seqCounter.Add(1)
	gen := w.genManager.Load() // Finding C: synchronized atomic generation read
	opID := fmt.Sprintf("chaos/%s/%08d", w.runID, seq)
	key := opID
	val := fmt.Sprintf("val-%s-%08d-gen%d", w.runID, seq, gen) // Finding K: deterministic value

	w.writesAttempted.Add(1)

	req := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  seq,
		Key:    []byte(key),
		Value:  []byte(val),
	}

	_ = conn.SetDeadline(time.Now().Add(1 * time.Second))

	if err := transport.WriteRequest(conn, req); err != nil {
		w.writesFailed.Add(1)
		return
	}

	resp, err := transport.ReadResponse(conn)
	if err != nil {
		w.writesFailed.Add(1)
		return
	}

	if resp.Status == transport.StatusOk {
		rec := AckRecord{
			OpID:         opID,
			SeqNum:       seq,
			Key:          key,
			Value:        val,
			RequestID:    seq,
			Generation:   gen,
			Timestamp:    time.Now().UTC(),
			Order:        int(w.writesAcked.Add(1)),
			ClientStatus: "StatusOk",
		}
		// Finding A: fail-closed signaling instead of panic
		if err := w.ledger.RecordAck(rec); err != nil {
			w.reportFatalError(err)
			return
		}
		select {
		case w.ackNotifyCh <- struct{}{}:
		default:
		}
	} else {
		w.writesFailed.Add(1)
	}
}

// PauseAndWait enforces true quiescence (Finding B).
// When this returns, no worker is dialing, writing, reading, reconnecting, or recording an ACK.
func (w *ContinuousWriter) PauseAndWait() {
	w.pauseMu.Lock()
	w.isPaused.Store(true)
	w.pauseMu.Unlock()

	// Sever all active TCP sockets so any in-flight Read/Write returns immediately
	w.connMu.Lock()
	for c := range w.activeConns {
		_ = c.Close()
	}
	w.connMu.Unlock()

	// Wait until all active operations have exited executeSingleWrite()
	w.activeOps.Wait()
}

func (w *ContinuousWriter) Resume() {
	w.pauseMu.Lock()
	w.isPaused.Store(false)
	w.pauseCond.Broadcast()
	w.pauseMu.Unlock()
}

func (w *ContinuousWriter) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
		w.pauseMu.Lock()
		w.isPaused.Store(false)
		w.pauseCond.Broadcast()
		w.pauseMu.Unlock()

		w.connMu.Lock()
		for c := range w.activeConns {
			_ = c.Close()
		}
		w.connMu.Unlock()

		w.doneWg.Wait()
	})
}

func (w *ContinuousWriter) AcksCount() uint64 {
	return w.writesAcked.Load()
}

func (w *ContinuousWriter) AttemptedCount() uint64 {
	return w.writesAttempted.Load()
}

func (w *ContinuousWriter) InterruptedCount() uint64 {
	return w.writesFailed.Load()
}

// verifySnapshot queries every acknowledged operation over TCP and asserts zero acknowledged write loss.
func verifySnapshot(t *testing.T, addr string, snapshot []AckRecord, gen int64, seed int64, pid int, cycle int) int {
	t.Helper()
	if len(snapshot) == 0 {
		return 0
	}

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect for verification on %s: %v", addr, err)
	}
	defer conn.Close()

	verifiedCount := 0
	for _, rec := range snapshot {
		req := &transport.Request{
			OpCode: transport.OpGet,
			SeqID:  rec.SeqNum,
			Key:    []byte(rec.Key),
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if err := transport.WriteRequest(conn, req); err != nil {
			t.Fatalf("verification WriteRequest failed for key %s: %v", rec.Key, err)
		}
		resp, err := transport.ReadResponse(conn)
		if err != nil {
			t.Fatalf("verification ReadResponse failed for key %s: %v", rec.Key, err)
		}
		if resp.Status != transport.StatusOk {
			t.Fatalf("DURABILITY VIOLATION: seed=%d gen=%d cycle=%d op_id=%s seq=%d key=%s expected_val=%s observed_status=%s observed_val=%s daemon_pid=%d ack_ledger_size=%d",
				seed, gen, cycle, rec.OpID, rec.SeqNum, rec.Key, rec.Value, resp.Status, string(resp.Value), pid, len(snapshot))
		}
		if string(resp.Value) != rec.Value {
			t.Fatalf("DURABILITY DATA CORRUPTION: seed=%d gen=%d cycle=%d op_id=%s seq=%d key=%s expected_val=%s observed_status=%s observed_val=%s daemon_pid=%d ack_ledger_size=%d",
				seed, gen, cycle, rec.OpID, rec.SeqNum, rec.Key, rec.Value, resp.Status, string(resp.Value), pid, len(snapshot))
		}
		verifiedCount++
	}
	return verifiedCount
}

// TestSIGKILLChaos implements P18-S01-M02: Abrupt SIGKILL Chaos Monkey Loop.
// Continuously writes data while sending random SIGKILL signals; asserts zero acknowledged write loss.
func TestSIGKILLChaos(t *testing.T) {
	seed := int64(180102)
	if sEnv := os.Getenv("CHAOS_SEED"); sEnv != "" {
		if s, err := strconv.ParseInt(sEnv, 10, 64); err == nil {
			seed = s
		}
	}
	rng := rand.New(rand.NewSource(seed))

	const crashCycles = 5
	const minAcksPerCycle = 15
	const numWorkers = 2

	// Finding K: Deterministic run identifier from seed
	runID := fmt.Sprintf("run-%d", seed)

	// Step 1: Persistent Test Layout (Finding 20: 0700 permissions)
	testRoot := t.TempDir()
	daemonDataDir := filepath.Join(testRoot, "daemon-data")
	ackLedgerDir := filepath.Join(testRoot, "ack-ledger")
	binDir := filepath.Join(testRoot, "binary")
	binPath := filepath.Join(binDir, "lattice")

	if err := os.MkdirAll(binDir, 0700); err != nil {
		t.Fatalf("failed to create binDir: %v", err)
	}

	// Step 2: Build Real Production Daemon Binary (FINDING-FID-02: pass -race if race testing enabled)
	buildArgs := []string{"build"}
	if isRaceEnabled {
		buildArgs = append(buildArgs, "-race")
	}
	buildArgs = append(buildArgs, "-o", binPath, ".")
	buildCmd := exec.Command("go", buildArgs...)
	buildCmd.Dir = "."
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build production lattice binary: %v, output: %s", err, string(out))
	}

	// Step 3: Initialize Parent-Owned ACK Ledger
	ledger, err := newAckLedger(ackLedgerDir)
	if err != nil {
		t.Fatalf("failed to initialize ACK ledger: %v", err)
	}
	t.Cleanup(func() {
		_ = ledger.Close()
	})

	// Step 4: Start Daemon Generation 0 with ephemeral port (Finding J)
	var genManager atomic.Int64
	genManager.Store(0)

	var currentDaemonMu sync.Mutex
	var currentDaemon *DaemonProcess

	currentDaemon, err = startDaemonProcess(t, binPath, daemonDataDir, genManager.Load())
	if err != nil {
		t.Fatalf("failed to start initial daemon generation 0: %v", err)
	}

	// Finding I: Guaranteed cleanup on all paths
	t.Cleanup(func() {
		currentDaemonMu.Lock()
		defer currentDaemonMu.Unlock()
		if currentDaemon != nil && !currentDaemon.IsDead() {
			_ = currentDaemon.Kill()
		}
	})

	// Step 5: Start Continuous Writer with synchronized generation manager (Finding C)
	writer := newContinuousWriter(currentDaemon.Addr(), runID, ledger, &genManager)
	writer.Start(numWorkers)
	t.Cleanup(func() {
		writer.Stop()
	})

	t.Logf("=== Starting SIGKILL Chaos Monkey: seed=%d, cycles=%d, dataDir=%s ===", seed, crashCycles, daemonDataDir)

	// Step 6: Repeat N Crash Cycles
	for cycle := 1; cycle <= crashCycles; cycle++ {
		// Wait for active workload to accumulate enough ACKs
		targetAcks := writer.AcksCount() + minAcksPerCycle
		timeoutDeadline := time.Now().Add(5 * time.Second)
		for writer.AcksCount() < targetAcks && time.Now().Before(timeoutDeadline) {
			select {
			case <-writer.fatalErrCh:
				t.Fatalf("cycle %d: writer fatal error: %v", cycle, writer.FatalError())
			case <-writer.ackNotifyCh:
			case <-time.After(50 * time.Millisecond):
			}
		}
		if writer.FatalError() != nil {
			t.Fatalf("cycle %d: writer fatal error: %v", cycle, writer.FatalError())
		}
		if writer.AcksCount() < targetAcks {
			t.Fatalf("cycle %d: writer timed out accumulating %d acks (got %d)", cycle, targetAcks, writer.AcksCount())
		}

		// Choose pseudo-random crash interval to strike during active in-flight writes
		jitterMs := rng.Intn(25)
		time.Sleep(time.Duration(jitterMs) * time.Millisecond)

		currentDaemonMu.Lock()
		targetPID := currentDaemon.PID()
		currentDaemonMu.Unlock()

		// Send SIGKILL (kill -9) to the exact child PID
		currentDaemonMu.Lock()
		if err := currentDaemon.Kill(); err != nil {
			currentDaemonMu.Unlock()
			t.Fatalf("cycle %d: failed to kill child daemon PID %d: %v", cycle, targetPID, err)
		}
		// Finding H: verify process is dead via ProcessState and Wait
		if !currentDaemon.IsDead() {
			currentDaemonMu.Unlock()
			t.Fatalf("cycle %d: process %d still alive after SIGKILL", cycle, targetPID)
		}
		currentDaemonMu.Unlock()

		// Finding B: TRUE QUIESCENCE BARRIER
		writer.PauseAndWait()

		// Read ACK ledger directly from synced disk file for verification (FINDING-FID-01)
		snapshot, err := ledger.ReadRecordsFromDisk()
		if err != nil {
			t.Fatalf("cycle %d: failed to read ack records from disk: %v", cycle, err)
		}
		acksBeforeCrash := len(snapshot)

		// Finding C: increment generation under complete writer quiescence
		nextGen := genManager.Add(1)

		// Restart same daemon with same persistent data directory on ephemeral port (Finding J)
		currentDaemonMu.Lock()
		currentDaemon = restartDaemonProcess(t, binPath, daemonDataDir, nextGen)
		newAddr := currentDaemon.Addr()
		currentDaemonMu.Unlock()

		// Update writer destination address to new daemon listener
		writer.SetAddr(newAddr)

		// Primary Safety Oracle: Validate all previously acknowledged operations
		verified := verifySnapshot(t, newAddr, snapshot, nextGen, seed, targetPID, cycle)
		if verified != len(snapshot) {
			t.Fatalf("cycle %d: verified count mismatch: verified %d != snapshot %d", cycle, verified, len(snapshot))
		}

		t.Logf("Cycle %d (Gen %d -> Gen %d): PID %d killed with SIGKILL, acks_before_crash=%d, verified=%d, missing=0, incorrect=0",
			cycle, nextGen-1, nextGen, targetPID, acksBeforeCrash, verified)

		// Resume write workload
		writer.Resume()
	}

	// Step 7: Stop Workload cleanly
	writer.Stop()

	finalRecordedAcks := ledger.Count()
	finalSnapshot, err := ledger.ReadRecordsFromDisk()
	if err != nil {
		t.Fatalf("failed to read final ack records from disk: %v", err)
	}

	// Step 8: Final Restart & Oracle Verification
	currentDaemonMu.Lock()
	finalTargetPID := currentDaemon.PID()
	if err := currentDaemon.Kill(); err != nil {
		currentDaemonMu.Unlock()
		t.Fatalf("final kill failed: %v", err)
	}
	nextGen := genManager.Add(1)
	currentDaemon = restartDaemonProcess(t, binPath, daemonDataDir, nextGen)
	finalAddr := currentDaemon.Addr()
	currentDaemonMu.Unlock()

	finalVerified := verifySnapshot(t, finalAddr, finalSnapshot, nextGen, seed, finalTargetPID, crashCycles+1)

	// Step 9: Final Assertion: verified_acknowledged_writes == recorded_acknowledged_writes
	if finalVerified != finalRecordedAcks {
		t.Fatalf("FINAL DURABILITY VIOLATION: final verified (%d) != recorded ACKs (%d)", finalVerified, finalRecordedAcks)
	}

	t.Logf("=== SIGKILL Chaos Monkey Completed Successfully ===")
	t.Logf("Seed: %d", seed)
	t.Logf("Crash Cycles: %d", crashCycles)
	t.Logf("Writes Attempted: %d", writer.AttemptedCount())
	t.Logf("Writes Acknowledged: %d", writer.AcksCount())
	t.Logf("In-Flight / Interrupted Writes: %d", writer.InterruptedCount())
	t.Logf("Acknowledged Writes Verified: %d", finalVerified)
	t.Logf("Missing Acknowledged Writes: 0")
	t.Logf("Incorrect Acknowledged Values: 0")
}

// -----------------------------------------------------------------------------
// NEGATIVE SECURITY & INVARIANT REGRESSION TESTS (Finding 23)
// -----------------------------------------------------------------------------

// TestAckLedger_FailClosed proves that short writes or sync errors fail closed
// and never leave a phantom record in memory (Finding D).
func TestAckLedger_FailClosed(t *testing.T) {
	dir := t.TempDir()
	ledger, err := newAckLedger(dir)
	if err != nil {
		t.Fatalf("failed to create ledger: %v", err)
	}
	defer ledger.Close()

	// 1. Valid record succeeds
	rec1 := AckRecord{
		OpID:      "op-1",
		SeqNum:    1,
		Key:       "key-1",
		Value:     "val-1",
		Timestamp: time.Now().UTC(),
	}
	if err := ledger.RecordAck(rec1); err != nil {
		t.Fatalf("expected valid record to succeed: %v", err)
	}
	if ledger.Count() != 1 {
		t.Fatalf("expected count 1, got %d", ledger.Count())
	}

	// 2. Force file close underneath the ledger to simulate persistent write failure
	_ = ledger.file.Close()

	rec2 := AckRecord{
		OpID:      "op-2",
		SeqNum:    2,
		Key:       "key-2",
		Value:     "val-2",
		Timestamp: time.Now().UTC(),
	}
	err = ledger.RecordAck(rec2)
	if err == nil {
		t.Fatal("expected write error on closed file, got nil")
	}

	// Verify fail-closed invariant: memory indices and count must NOT contain rec2!
	if ledger.Count() != 1 {
		t.Fatalf("FAIL-CLOSED VIOLATION: expected count 1, got %d", ledger.Count())
	}
	snap := ledger.Snapshot()
	if len(snap) != 1 || snap[0].Key != "key-1" {
		t.Fatalf("FAIL-CLOSED VIOLATION: phantom record published in memory: %+v", snap)
	}
	if _, found := ledger.byKey["key-2"]; found {
		t.Fatalf("FAIL-CLOSED VIOLATION: byKey contains phantom key-2")
	}
}

// TestAckLedger_RejectsDuplicates verifies that duplicate OpID, SeqNum, or Key are rejected (Finding E).
func TestAckLedger_RejectsDuplicates(t *testing.T) {
	dir := t.TempDir()
	ledger, err := newAckLedger(dir)
	if err != nil {
		t.Fatalf("failed to create ledger: %v", err)
	}
	defer ledger.Close()

	rec1 := AckRecord{
		OpID:      "op-unique-1",
		SeqNum:    100,
		Key:       "key-unique-1",
		Value:     "val-1",
		Timestamp: time.Now().UTC(),
	}
	if err := ledger.RecordAck(rec1); err != nil {
		t.Fatalf("initial record failed: %v", err)
	}

	// Duplicate Key
	recDupKey := AckRecord{
		OpID:      "op-unique-2",
		SeqNum:    101,
		Key:       "key-unique-1",
		Value:     "val-dup",
		Timestamp: time.Now().UTC(),
	}
	if err := ledger.RecordAck(recDupKey); err == nil {
		t.Fatal("expected error on duplicate key, got nil")
	}

	// Duplicate OpID
	recDupOpID := AckRecord{
		OpID:      "op-unique-1",
		SeqNum:    102,
		Key:       "key-unique-3",
		Value:     "val-dup",
		Timestamp: time.Now().UTC(),
	}
	if err := ledger.RecordAck(recDupOpID); err == nil {
		t.Fatal("expected error on duplicate opID, got nil")
	}

	// Duplicate SeqNum
	recDupSeq := AckRecord{
		OpID:      "op-unique-4",
		SeqNum:    100,
		Key:       "key-unique-4",
		Value:     "val-dup",
		Timestamp: time.Now().UTC(),
	}
	if err := ledger.RecordAck(recDupSeq); err == nil {
		t.Fatal("expected error on duplicate seqNum, got nil")
	}

	if ledger.Count() != 1 {
		t.Fatalf("expected count 1 after rejected duplicates, got %d", ledger.Count())
	}
}

// TestContinuousWriter_QuiescenceBarrier verifies that PauseAndWait() prevents any
// worker from starting or completing writes while paused (Finding B).
func TestContinuousWriter_QuiescenceBarrier(t *testing.T) {
	dir := t.TempDir()
	ledger, err := newAckLedger(dir)
	if err != nil {
		t.Fatalf("failed to create ledger: %v", err)
	}
	defer ledger.Close()

	var gen atomic.Int64
	gen.Store(1)

	// Create a dummy TCP listener to accept writes
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	// Ingest dummy Put requests
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				for {
					req, err := transport.ReadRequest(c)
					if err != nil {
						return
					}
					resp := &transport.Response{
						OpCode: req.OpCode,
						Status: transport.StatusOk,
						SeqID:  req.SeqID,
					}
					if err := transport.WriteResponse(c, resp); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	cw := newContinuousWriter(ln.Addr().String(), "test-run", ledger, &gen)
	cw.Start(4)
	defer cw.Stop()

	// Wait for at least 10 ACKs
	deadline := time.Now().Add(2 * time.Second)
	for cw.AcksCount() < 10 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cw.AcksCount() < 10 {
		t.Fatalf("expected at least 10 acks before pause, got %d", cw.AcksCount())
	}

	// Trigger PauseAndWait
	cw.PauseAndWait()

	// Capture count post-quiescence
	countAtPause := cw.AcksCount()
	attemptsAtPause := cw.AttemptedCount()

	// Sleep 100ms and verify ZERO new operations occurred
	time.Sleep(100 * time.Millisecond)

	if cw.AcksCount() != countAtPause {
		t.Fatalf("QUIESCENCE VIOLATION: acks advanced during pause from %d to %d", countAtPause, cw.AcksCount())
	}
	if cw.AttemptedCount() != attemptsAtPause {
		t.Fatalf("QUIESCENCE VIOLATION: attempts advanced during pause from %d to %d", attemptsAtPause, cw.AttemptedCount())
	}

	// Resume and verify writes continue
	cw.Resume()
	deadline = time.Now().Add(2 * time.Second)
	for cw.AcksCount() < countAtPause+10 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cw.AcksCount() < countAtPause+10 {
		t.Fatalf("expected acks to resume after unpause, got %d", cw.AcksCount())
	}
}

// TestContinuousWriter_PropagatesFatalErrorWithoutOrphan verifies that a ledger failure
// does not panic in the worker and is observed by the supervisor (Finding A).
func TestContinuousWriter_PropagatesFatalErrorWithoutOrphan(t *testing.T) {
	dir := t.TempDir()
	ledger, err := newAckLedger(dir)
	if err != nil {
		t.Fatalf("failed to create ledger: %v", err)
	}

	var gen atomic.Int64
	gen.Store(1)

	// Echo server responding with StatusOk
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				for {
					req, err := transport.ReadRequest(c)
					if err != nil {
						return
					}
					resp := &transport.Response{
						OpCode: req.OpCode,
						Status: transport.StatusOk,
						SeqID:  req.SeqID,
					}
					_ = transport.WriteResponse(c, resp)
				}
			}(conn)
		}
	}()

	// Close the ledger file beforehand so RecordAck fails immediately
	_ = ledger.Close()

	cw := newContinuousWriter(ln.Addr().String(), "fatal-test", ledger, &gen)
	cw.Start(2)
	defer cw.Stop()

	// Wait for fatal error signal
	select {
	case <-cw.fatalErrCh:
		// Fatal error observed cleanly without panic
		err := cw.FatalError()
		if err == nil {
			t.Fatal("expected non-nil fatal error, got nil")
		}
		t.Logf("Observed fatal error as expected: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for fatal error propagation")
	}
}

// TestDaemon_ChildEnvironmentIsolation proves sensitive parent environment variables
// are not propagated to the child process (Finding G) and HOME/TMPDIR are sandboxed (FINDING-SEC-01).
func TestDaemon_ChildEnvironmentIsolation(t *testing.T) {
	// Set dummy sensitive secret in parent
	secretKey := "LATTICE_TEST_SECRET_TOKEN"
	secretVal := "secret-super-sensitive-12345"
	t.Setenv(secretKey, secretVal)

	testRoot := t.TempDir()
	env := buildChildEnv(testRoot)
	var foundHome, foundTmp bool
	for _, entry := range env {
		if strings.HasPrefix(entry, secretKey+"=") {
			t.Fatalf("ENVIRONMENT ISOLATION VIOLATION: secret %s leaked into child environment!", secretKey)
		}
		if strings.HasPrefix(entry, "HOME=") {
			foundHome = true
			if !strings.HasPrefix(entry, "HOME="+testRoot) {
				t.Fatalf("HOME not sandboxed in testRoot: %s", entry)
			}
		}
		if strings.HasPrefix(entry, "TMPDIR=") {
			foundTmp = true
			if !strings.HasPrefix(entry, "TMPDIR="+testRoot) {
				t.Fatalf("TMPDIR not sandboxed in testRoot: %s", entry)
			}
		}
	}
	if !foundHome {
		t.Fatalf("expected HOME to be configured in child env")
	}
	if !foundTmp {
		t.Fatalf("expected TMPDIR to be configured in child env")
	}
}

// TestDaemon_ProcessIdentityAuthoritative verifies that child process state is
// confirmed via ProcessState.Exited() after SIGKILL (Finding H).
func TestDaemon_ProcessIdentityAuthoritative(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "lattice")

	buildArgs := []string{"build"}
	if isRaceEnabled {
		buildArgs = append(buildArgs, "-race")
	}
	buildArgs = append(buildArgs, "-o", binPath, ".")
	buildCmd := exec.Command("go", buildArgs...)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build binary: %v, output: %s", err, string(out))
	}

	dataDir := t.TempDir()
	dp, err := startDaemonProcess(t, binPath, dataDir, 0)
	if err != nil {
		t.Fatalf("failed to start daemon: %v", err)
	}

	pid := dp.PID()
	if pid <= 0 {
		t.Fatalf("invalid child PID: %d", pid)
	}

	if err := dp.Kill(); err != nil {
		t.Fatalf("failed to kill daemon: %v", err)
	}

	if !dp.IsDead() {
		t.Fatalf("expected dp.IsDead() == true after Kill()")
	}

	if dp.cmd.ProcessState == nil {
		t.Fatalf("expected ProcessState != nil")
	}
	if ws, ok := dp.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || (!ws.Signaled() && !ws.Exited()) {
		t.Fatalf("expected process to have terminated, got: %v", dp.cmd.ProcessState)
	}
}

// TestWAL_DeterministicTornTailRecovery proves that a truncated torn tail at the end
// of a WAL segment is detected, cleanly truncated, and valid preceding records survive (Finding P).
func TestWAL_DeterministicTornTailRecovery(t *testing.T) {
	walDir := t.TempDir()
	if err := os.MkdirAll(wal.Dir(walDir), 0700); err != nil {
		t.Fatalf("failed to create wal dir: %v", err)
	}
	segmentPath := wal.SegmentPath(walDir, 1)

	// 1. Write 3 valid records to a real WAL writer
	w, err := wal.OpenWriter(segmentPath)
	if err != nil {
		t.Fatalf("failed to open WAL writer: %v", err)
	}

	for i := uint64(1); i <= 3; i++ {
		rec := wal.Record{
			SeqNum: binary.SeqNum(i),
			Type:   wal.RecordTypePut,
			Key:    []byte(fmt.Sprintf("k%d", i)),
			Value:  []byte(fmt.Sprintf("v%d", i)),
		}
		if err := w.Append(rec); err != nil {
			t.Fatalf("failed to append record %d: %v", i, err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("failed to sync WAL: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close WAL writer: %v", err)
	}

	// 2. Simulate torn tail: append 7 bytes of a partial incomplete record frame
	f, err := os.OpenFile(segmentPath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("failed to open segment for corrupting tail: %v", err)
	}
	tornBytes := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03}
	if _, err := f.Write(tornBytes); err != nil {
		t.Fatalf("failed to append torn bytes: %v", err)
	}
	_ = f.Sync()
	_ = f.Close()

	statCorrupt, err := os.Stat(segmentPath)
	if err != nil {
		t.Fatalf("failed to stat corrupt segment: %v", err)
	}

	// 3. Call wal.RecoverSegment to physically truncate the torn tail in-place (FINDING-FID-03)
	res, err := wal.RecoverSegment(segmentPath)
	if err != nil {
		t.Fatalf("failed to recover segment: %v", err)
	}
	if !res.Truncated {
		t.Fatalf("expected res.Truncated == true, got false")
	}
	if res.ValidRecords != 3 {
		t.Fatalf("expected 3 valid records, got %d", res.ValidRecords)
	}

	statRecovered, err := os.Stat(segmentPath)
	if err != nil {
		t.Fatalf("failed to stat recovered segment: %v", err)
	}
	if statRecovered.Size() >= statCorrupt.Size() {
		t.Fatalf("expected recovered file size (%d) < corrupt file size (%d)", statRecovered.Size(), statCorrupt.Size())
	}
	if statRecovered.Size() != res.RecoveredOffset {
		t.Fatalf("expected recovered file size (%d) == res.RecoveredOffset (%d)", statRecovered.Size(), res.RecoveredOffset)
	}

	// 4. Open Reader and verify recovery: records 1..3 must be valid, cleanly reading up to io.EOF
	reader, err := wal.OpenReader(segmentPath)
	if err != nil {
		t.Fatalf("failed to open WAL reader on recovered segment: %v", err)
	}
	defer reader.Close()

	readCount := 0
	for {
		rec, err := reader.Next()
		if err != nil {
			break
		}
		readCount++
		expectedKey := fmt.Sprintf("k%d", readCount)
		if string(rec.Key) != expectedKey {
			t.Fatalf("record %d key mismatch: expected %s, got %s", readCount, expectedKey, string(rec.Key))
		}
	}

	if readCount != 3 {
		t.Fatalf("TORN-TAIL RECOVERY VIOLATION: expected 3 valid records recovered, got %d", readCount)
	}
	t.Logf("Deterministic torn tail recovery verified: 3 valid records recovered, physical file truncated from %d to %d bytes.",
		statCorrupt.Size(), statRecovered.Size())
}

// TestAckLedger_ReadRecordsFromDisk verifies that ReadRecordsFromDisk accurately parses
// records directly from disk and handles empty ledgers cleanly (FINDING-FID-01).
func TestAckLedger_ReadRecordsFromDisk(t *testing.T) {
	dir := t.TempDir()
	ledger, err := newAckLedger(dir)
	if err != nil {
		t.Fatalf("failed to create ack ledger: %v", err)
	}
	defer ledger.Close()

	// Initial empty check
	recs, err := ledger.ReadRecordsFromDisk()
	if err != nil {
		t.Fatalf("unexpected error on empty ledger: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected 0 records, got %d", len(recs))
	}

	// Record 3 writes
	for i := 1; i <= 3; i++ {
		rec := AckRecord{
			OpID:         fmt.Sprintf("op-%d", i),
			SeqNum:       uint64(i),
			Key:          fmt.Sprintf("k%d", i),
			Value:        fmt.Sprintf("v%d", i),
			RequestID:    uint64(i),
			Generation:   0,
			Timestamp:    time.Now(),
			Order:        i,
			ClientStatus: "OK",
		}
		if err := ledger.RecordAck(rec); err != nil {
			t.Fatalf("RecordAck failed: %v", err)
		}
	}

	recs, err = ledger.ReadRecordsFromDisk()
	if err != nil {
		t.Fatalf("ReadRecordsFromDisk failed: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("expected 3 records, got %d", len(recs))
	}
	for i, r := range recs {
		expectedOpID := fmt.Sprintf("op-%d", i+1)
		if r.OpID != expectedOpID {
			t.Fatalf("record %d OpID mismatch: expected %s, got %s", i, expectedOpID, r.OpID)
		}
	}
}
