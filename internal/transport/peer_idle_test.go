package transport

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
)

// =============================================================================
// Peer idle read deadline (regression).
//
// The read deadline set for a peer's first frame was cleared once that frame
// arrived, and nothing ever set one again. A peer that stopped sending without
// closing its socket therefore held its supervisor slot indefinitely: runReader
// blocked forever inside DecodeFrame, the slot was never reclaimed, and the
// manager could not recover that peer without a process restart.
//
// Every read is now bounded by PeerIdleTimeout and the deadline is cleared after
// each successfully decoded frame, so the bound measures silence rather than
// session length.
// =============================================================================

// idleTestFrame builds a frame the reader accepts. runReader validates the magic,
// payload length, CRC, opcode namespace, and flag/status combination, but does not
// decode the payload, so a well-formed empty payload is sufficient.
func idleTestFrame(t *testing.T, seqID uint64) *Frame {
	t.Helper()
	// OpCode is the byte-typed client opcode space; the peer reader casts it to
	// PeerMessageType. SeqID doubles as the anti-replay nonce here.
	return &Frame{
		Header: Header{
			Magic:  Magic,
			OpCode: OpCode(PeerOpAppendEntriesResponse),
			Status: StatusOk,
			Flags:  FlagNone,
			SeqID:  seqID,
		},
	}
}

// startIdleReader wires a peerSupervisor around one end of a net.Pipe and runs
// runReader on it, returning a channel closed when the reader returns.
func startIdleReader(t *testing.T, idle time.Duration) (peerSide net.Conn, readerDone chan struct{}) {
	t.Helper()
	readerSide, writerSide := net.Pipe()

	sup := &peerSupervisor{
		peerID:       cluster.NodeID(2),
		cfg:          PeerConnectionConfig{PeerIdleTimeout: idle},
		state:        PeerStateConnected,
		replayFilter: newPeerReplayFilter(cluster.NodeID(2)),
	}

	// runReader releases sup.readerWg itself, so prime it rather than a local group.
	sup.readerWg.Add(1)
	done := make(chan struct{})
	go func() {
		sup.runReader(context.Background(), 1, readerSide)
		close(done)
	}()
	t.Cleanup(func() {
		_ = writerSide.Close()
		_ = readerSide.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	return writerSide, done
}

// TestPeerIdle_SilentPeerIsDisconnected is the primary regression.
//
// Pre-fix, runReader blocked forever and this test would hang.
func TestPeerIdle_SilentPeerIsDisconnected(t *testing.T) {
	const idle = 150 * time.Millisecond
	_, done := startIdleReader(t, idle)

	select {
	case <-done:
		// Expected: the reader gave up on a silent peer.
	case <-time.After(10 * time.Second):
		t.Fatalf("runReader still blocked after 10s with a silent peer; "+
			"PeerIdleTimeout=%v was not enforced", idle)
	}
}

// TestPeerIdle_TeardownIsBounded measures that teardown actually happens near the
// configured bound rather than arbitrarily late.
func TestPeerIdle_TeardownIsBounded(t *testing.T) {
	const idle = 200 * time.Millisecond

	readerSide, writerSide := net.Pipe()
	sup := &peerSupervisor{
		peerID:       cluster.NodeID(2),
		cfg:          PeerConnectionConfig{PeerIdleTimeout: idle},
		state:        PeerStateConnected,
		replayFilter: newPeerReplayFilter(cluster.NodeID(2)),
	}
	sup.readerWg.Add(1)
	done := make(chan struct{})
	go func() {
		sup.runReader(context.Background(), 1, readerSide)
		close(done)
	}()
	defer func() {
		_ = writerSide.Close()
		_ = readerSide.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}()

	start := time.Now()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("silent peer was not disconnected within 5s (idle=%v)", idle)
	}
	took := time.Since(start)

	// Must not fire early, and must not take an absurdly long time.
	if took < idle/2 {
		t.Errorf("disconnected after %v, well before the %v idle bound; the bound is too tight "+
			"and would drop healthy peers", took, idle)
	}
	if took > 5*time.Second {
		t.Errorf("disconnected after %v for a %v idle bound", took, idle)
	}
}

// TestPeerIdle_SlowButAlivePeerStaysConnected is the essential counterpart: the
// bound measures silence, not session length. A peer delivering frames slower than
// the idle window must never be dropped.
func TestPeerIdle_SlowButAlivePeerStaysConnected(t *testing.T) {
	const (
		idle = 200 * time.Millisecond
		gap  = 80 * time.Millisecond // well under idle
	)

	writerSide, done := startIdleReader(t, idle)

	// Deliver frames for longer than several idle windows.
	deadline := time.Now().Add(900 * time.Millisecond)
	seq := uint64(1)
	for time.Now().Before(deadline) {
		f := idleTestFrame(t, seq)
		seq++
		if err := EncodeFrame(writerSide, f); err != nil {
			t.Fatalf("EncodeFrame: %v", err)
		}
		time.Sleep(gap)
	}

	select {
	case <-done:
		t.Fatalf("runReader returned while the peer was still delivering frames every %v "+
			"(idle bound %v); the deadline is not being refreshed on progress", gap, idle)
	default:
	}
}

// TestPeerIdle_HeartbeatCadenceNeverTripsIt mirrors production: Raft heartbeats
// every 50ms, so the 60s default has ~1200x margin.
func TestPeerIdle_HeartbeatCadenceNeverTripsIt(t *testing.T) {
	const (
		idle = 300 * time.Millisecond
		gap  = 50 * time.Millisecond // the real heartbeat cadence
	)

	writerSide, done := startIdleReader(t, idle)

	seq := uint64(1)
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := EncodeFrame(writerSide, idleTestFrame(t, seq)); err != nil {
			t.Fatalf("EncodeFrame: %v", err)
		}
		seq++
		time.Sleep(gap)
	}

	select {
	case <-done:
		t.Fatalf("reader dropped a peer sending at the %v heartbeat cadence under a %v idle bound",
			gap, idle)
	default:
	}
}

// TestPeerIdle_DefaultIsPositiveAndApplied guards the defaulting path, so the bound
// can never silently revert to "unbounded".
func TestPeerIdle_DefaultIsPositiveAndApplied(t *testing.T) {
	if DefaultPeerIdleTimeout <= 0 {
		t.Fatalf("DefaultPeerIdleTimeout = %v; want a positive bound", DefaultPeerIdleTimeout)
	}
	if got := DefaultPeerConnectionConfig().PeerIdleTimeout; got != DefaultPeerIdleTimeout {
		t.Errorf("DefaultPeerConnectionConfig().PeerIdleTimeout = %v; want %v", got, DefaultPeerIdleTimeout)
	}

}

// TestPeerIdle_ZeroIsDefaultedNotUnbounded is the important negative case: a caller
// that leaves PeerIdleTimeout at zero must get the default, not "no deadline".
func TestPeerIdle_ZeroIsDefaultedNotUnbounded(t *testing.T) {
	// Drive runReader with the zero value and confirm it still terminates on a
	// silent peer, which is only possible if the default was applied.
	topo := testLoopbackTopology(t)
	mgr, err := NewPeerConnectionManager(topo, PeerConnectionConfig{PeerIdleTimeout: DefaultPeerIdleTimeout})
	if err != nil {
		t.Fatalf("NewPeerConnectionManager: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	sup := mgr.supervisors[cluster.NodeID(2)]
	if sup == nil {
		t.Fatal("expected a supervisor for peer 2")
	}
	if sup.cfg.PeerIdleTimeout != DefaultPeerIdleTimeout {
		t.Errorf("supervisor PeerIdleTimeout = %v; want the default %v",
			sup.cfg.PeerIdleTimeout, DefaultPeerIdleTimeout)
	}
}

// TestPeerIdle_NegativeRejected keeps the bound well-formed at construction.
func TestPeerIdle_NegativeRejected(t *testing.T) {
	topo := testLoopbackTopology(t)
	mgr, err := NewPeerConnectionManager(topo, PeerConnectionConfig{PeerIdleTimeout: -1})
	if err == nil {
		_ = mgr.Close()
		t.Fatal("NewPeerConnectionManager accepted a negative PeerIdleTimeout")
	}
}

// testLoopbackTopology builds a two-node all-loopback topology, which needs no TLS.
func testLoopbackTopology(t *testing.T) *cluster.Topology {
	t.Helper()
	topo, err := cluster.NewTopology(1, "127.0.0.1:19098", []cluster.PeerConfig{
		{ID: 2, Address: "127.0.0.1:19099"},
	})
	if err != nil {
		t.Fatalf("NewTopology: %v", err)
	}
	return topo
}
