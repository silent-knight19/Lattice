package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/transport"
)

// AckRecord models an immutable acknowledged operation stored outside the daemon.
type AckRecord struct {
	OpID         string    `json:"op_id"`
	SeqNum       uint64    `json:"seq_num"`
	Key          string    `json:"key"`
	Value        string    `json:"value"`
	RequestID    uint64    `json:"request_id"`
	Generation   int       `json:"generation"`
	Timestamp    time.Time `json:"timestamp"`
	Order        int       `json:"order"`
	ClientStatus string    `json:"client_status"`
}

// AckLedger is the parent-supervised, crash-independent persistence oracle for acknowledged writes.
// It resides outside the daemon data directory and cannot be modified or truncated by the daemon.
type AckLedger struct {
	mu      sync.RWMutex
	records []AckRecord
	byKey   map[string]AckRecord
	file    *os.File
}

func newAckLedger(dir string) (*AckLedger, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create ack-ledger dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "ack_ledger.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open ack ledger file: %w", err)
	}
	return &AckLedger{
		byKey: make(map[string]AckRecord),
		file:  f,
	}, nil
}

func (l *AckLedger) RecordAck(rec AckRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.records = append(l.records, rec)
	l.byKey[rec.Key] = rec

	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := l.file.Write(data); err != nil {
		return err
	}
	return l.file.Sync()
}

func (l *AckLedger) Snapshot() []AckRecord {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cp := make([]AckRecord, len(l.records))
	copy(cp, l.records)
	return cp
}

func (l *AckLedger) Count() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.records)
}

func (l *AckLedger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// DaemonProcess encapsulates a real compiled Lattice daemon subprocess supervised by the test.
type DaemonProcess struct {
	cmd       *exec.Cmd
	pid       int
	port      int
	dataDir   string
	binPath   string
	stderrBuf *bytes.Buffer
	doneCh    chan error
	isDead    atomic.Bool
	mu        sync.Mutex
}

// startDaemonProcess spawns the real Lattice binary, drains stdout/stderr, and probes for TCP readiness.
func startDaemonProcess(t *testing.T, binPath, dataDir string, port int, gen int) (*DaemonProcess, error) {
	cmd := exec.Command(binPath, "--data-dir", dataDir, "--port", strconv.Itoa(port))
	cmd.Env = os.Environ()

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start daemon process: %w", err)
	}

	dp := &DaemonProcess{
		cmd:       cmd,
		pid:       cmd.Process.Pid,
		port:      port,
		dataDir:   dataDir,
		binPath:   binPath,
		stderrBuf: &stderrBuf,
		doneCh:    make(chan error, 1),
	}

	go func() {
		dp.doneCh <- cmd.Wait()
		dp.isDead.Store(true)
	}()

	readyCh := make(chan struct{})
	var once sync.Once
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			text := scanner.Text()
			if strings.Contains(text, "server listening on") {
				once.Do(func() { close(readyCh) })
			}
		}
	}()

	select {
	case <-readyCh:
		// Daemon reported server listening state.
	case err := <-dp.doneCh:
		return nil, fmt.Errorf("daemon exited prematurely (err: %v, stderr: %s)", err, dp.stderrBuf.String())
	case <-time.After(5 * time.Second):
		_ = dp.Kill()
		return nil, fmt.Errorf("timed out waiting for listening announcement (stderr: %s)", dp.stderrBuf.String())
	}

	// Verify listener readiness via explicit TCP connection probe.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	probeDeadline := time.Now().Add(3 * time.Second)
	var probeConn net.Conn
	var probeErr error
	for time.Now().Before(probeDeadline) {
		probeConn, probeErr = net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if probeErr == nil {
			_ = probeConn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if probeErr != nil {
		_ = dp.Kill()
		return nil, fmt.Errorf("daemon announced listening but TCP probe failed: %w", probeErr)
	}

	return dp, nil
}

// Kill delivers an abrupt SIGKILL (equivalent to kill -9) to the exact child process.
// It waits for process termination and reaps the exit status.
func (dp *DaemonProcess) Kill() error {
	dp.mu.Lock()
	defer dp.mu.Unlock()

	if dp.isDead.Load() {
		return nil
	}
	if dp.cmd == nil || dp.cmd.Process == nil {
		return nil
	}

	// os.Process.Kill sends SIGKILL on Unix-like systems.
	if err := dp.cmd.Process.Kill(); err != nil {
		// If already finished, that is acceptable
		if !strings.Contains(err.Error(), "process already finished") {
			return err
		}
	}

	select {
	case <-dp.doneCh:
		dp.isDead.Store(true)
		return nil
	case <-time.After(5 * time.Second):
		dp.isDead.Store(true)
		return fmt.Errorf("process %d failed to terminate within 5s after SIGKILL", dp.pid)
	}
}

// IsDead asserts that the process has completely terminated at the OS level.
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

// PID returns the OS process ID of the daemon child.
func (dp *DaemonProcess) PID() int {
	return dp.pid
}

// restartDaemonProcess restarts the daemon against the same persistent data directory.
func restartDaemonProcess(t *testing.T, binPath, dataDir string, port int, gen int) *DaemonProcess {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		dp, err := startDaemonProcess(t, binPath, dataDir, port, gen)
		if err == nil {
			return dp
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("failed to restart daemon generation %d within 10s: %v", gen, lastErr)
	return nil
}

// ContinuousWriter continuously issues Put operations over the real TCP client path.
type ContinuousWriter struct {
	addr            string
	runID           string
	ledger          *AckLedger
	genGetter       func() int
	isPaused        atomic.Bool
	stopOnce        sync.Once
	stopCh          chan struct{}
	doneWg          sync.WaitGroup
	seqCounter      atomic.Uint64
	writesAttempted atomic.Uint64
	writesAcked     atomic.Uint64
	writesFailed    atomic.Uint64 // interrupted / broken connection / no-ack
	ackNotifyCh     chan struct{}
}

func newContinuousWriter(addr, runID string, ledger *AckLedger, genGetter func() int) *ContinuousWriter {
	return &ContinuousWriter{
		addr:        addr,
		runID:       runID,
		ledger:      ledger,
		genGetter:   genGetter,
		stopCh:      make(chan struct{}),
		ackNotifyCh: make(chan struct{}, 1024),
	}
}

func (w *ContinuousWriter) Start(workers int) {
	for i := 0; i < workers; i++ {
		w.doneWg.Add(1)
		go w.workerLoop()
	}
}

func (w *ContinuousWriter) workerLoop() {
	defer w.doneWg.Done()
	var conn net.Conn
	var err error

	for {
		select {
		case <-w.stopCh:
			if conn != nil {
				_ = conn.Close()
			}
			return
		default:
		}

		if w.isPaused.Load() {
			if conn != nil {
				_ = conn.Close()
				conn = nil
			}
			time.Sleep(5 * time.Millisecond)
			continue
		}

		if conn == nil {
			conn, err = net.DialTimeout("tcp", w.addr, 200*time.Millisecond)
			if err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
		}

		seq := w.seqCounter.Add(1)
		gen := w.genGetter()
		opID := fmt.Sprintf("chaos/%s/%08d", w.runID, seq)
		key := opID
		val := fmt.Sprintf("val-%s-%08d-gen%d-t%d", w.runID, seq, gen, time.Now().UnixNano())

		w.writesAttempted.Add(1)

		req := &transport.Request{
			OpCode: transport.OpPut,
			SeqID:  seq,
			Key:    []byte(key),
			Value:  []byte(val),
		}

		_ = conn.SetDeadline(time.Now().Add(1 * time.Second))

		if err := transport.WriteRequest(conn, req); err != nil {
			// Write interrupted by SIGKILL or connection break.
			w.writesFailed.Add(1)
			_ = conn.Close()
			conn = nil
			continue
		}

		resp, err := transport.ReadResponse(conn)
		if err != nil {
			// Response interrupted by SIGKILL.
			w.writesFailed.Add(1)
			_ = conn.Close()
			conn = nil
			continue
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
			if err := w.ledger.RecordAck(rec); err != nil {
				panic(fmt.Sprintf("fatal ack ledger failure: %v", err))
			}
			select {
			case w.ackNotifyCh <- struct{}{}:
			default:
			}
		} else {
			w.writesFailed.Add(1)
		}
	}
}

func (w *ContinuousWriter) Pause() {
	w.isPaused.Store(true)
}

func (w *ContinuousWriter) Resume() {
	w.isPaused.Store(false)
}

func (w *ContinuousWriter) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
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
func verifySnapshot(t *testing.T, addr string, snapshot []AckRecord, gen int, seed int64, pid int, cycle int) int {
	t.Helper()
	if len(snapshot) == 0 {
		return 0
	}

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect for verification: %v", err)
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

	runID := fmt.Sprintf("run-%d-%d", seed, time.Now().UnixNano()%100000)

	// Step 1: Persistent Test Layout
	// test-root/
	//   daemon-data/
	//   ack-ledger/
	//   binary/
	testRoot := t.TempDir()
	daemonDataDir := filepath.Join(testRoot, "daemon-data")
	ackLedgerDir := filepath.Join(testRoot, "ack-ledger")
	binDir := filepath.Join(testRoot, "binary")
	binPath := filepath.Join(binDir, "lattice")

	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("failed to create binDir: %v", err)
	}

	// Step 2: Build Real Production Daemon Binary
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
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

	// Step 4: Allocate Port for Daemon
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// Step 5: Start Daemon Generation 0
	currentGen := 0
	currentDaemon, err := startDaemonProcess(t, binPath, daemonDataDir, port, currentGen)
	if err != nil {
		t.Fatalf("failed to start initial daemon generation 0: %v", err)
	}
	t.Cleanup(func() {
		if currentDaemon != nil && !currentDaemon.IsDead() {
			_ = currentDaemon.Kill()
		}
	})

	// Step 6: Start Continuous Writer
	writer := newContinuousWriter(addr, runID, ledger, func() int { return currentGen })
	writer.Start(numWorkers)
	t.Cleanup(func() {
		writer.Stop()
	})

	t.Logf("=== Starting SIGKILL Chaos Monkey: seed=%d, cycles=%d, dataDir=%s ===", seed, crashCycles, daemonDataDir)

	// Step 7: Repeat N Crash Cycles
	for cycle := 1; cycle <= crashCycles; cycle++ {
		// Wait for active workload to accumulate enough ACKs
		targetAcks := writer.AcksCount() + minAcksPerCycle
		timeoutDeadline := time.Now().Add(5 * time.Second)
		for writer.AcksCount() < targetAcks && time.Now().Before(timeoutDeadline) {
			select {
			case <-writer.ackNotifyCh:
			case <-time.After(50 * time.Millisecond):
			}
		}
		if writer.AcksCount() < targetAcks {
			t.Fatalf("cycle %d: writer timed out accumulating %d acks (got %d)", cycle, targetAcks, writer.AcksCount())
		}

		// Choose pseudo-random crash interval to strike during active in-flight writes
		jitterMs := rng.Intn(25)
		time.Sleep(time.Duration(jitterMs) * time.Millisecond)

		targetPID := currentDaemon.PID()
		acksBeforeCrash := ledger.Count()

		// Send SIGKILL (kill -9) to the exact child PID
		if err := currentDaemon.Kill(); err != nil {
			t.Fatalf("cycle %d: failed to kill child daemon PID %d: %v", cycle, targetPID, err)
		}

		// Verify process is actually dead
		if !currentDaemon.IsDead() {
			t.Fatalf("cycle %d: process %d still alive after SIGKILL", cycle, targetPID)
		}

		// Pause writer while recovering and validating
		writer.Pause()

		// Snapshot ACK ledger for verification
		snapshot := ledger.Snapshot()

		// Restart same daemon with same persistent data directory
		currentGen++
		currentDaemon = restartDaemonProcess(t, binPath, daemonDataDir, port, currentGen)

		// Primary Safety Oracle: Validate all previously acknowledged operations
		verified := verifySnapshot(t, addr, snapshot, currentGen, seed, targetPID, cycle)
		if verified != len(snapshot) {
			t.Fatalf("cycle %d: verified count mismatch: verified %d != snapshot %d", cycle, verified, len(snapshot))
		}

		t.Logf("Cycle %d (Gen %d -> Gen %d): PID %d killed with SIGKILL, acks_before_crash=%d, verified=%d, missing=0, incorrect=0",
			cycle, currentGen-1, currentGen, targetPID, acksBeforeCrash, verified)

		// Resume write workload
		writer.Resume()
	}

	// Step 8: Stop Workload
	writer.Stop()

	finalRecordedAcks := ledger.Count()
	finalSnapshot := ledger.Snapshot()

	// Step 9: Final Restart & Oracle Verification
	finalTargetPID := currentDaemon.PID()
	if err := currentDaemon.Kill(); err != nil {
		t.Fatalf("final kill failed: %v", err)
	}
	currentGen++
	currentDaemon = restartDaemonProcess(t, binPath, daemonDataDir, port, currentGen)

	finalVerified := verifySnapshot(t, addr, finalSnapshot, currentGen, seed, finalTargetPID, crashCycles+1)

	// Step 10: Final Assertion: verified_acknowledged_writes == recorded_acknowledged_writes
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
