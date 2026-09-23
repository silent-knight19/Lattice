package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

const (
	// ExitSuccess indicates normal, clean termination or graceful shutdown.
	ExitSuccess = 0

	// ExitConfigError indicates invalid command line arguments or malformed configuration.
	ExitConfigError = 1

	// ExitStartupError indicates failure during storage engine recovery or TCP listener binding.
	ExitStartupError = 2

	// ExitRuntimeError indicates an unexpected runtime failure during active serving.
	ExitRuntimeError = 3

	// ExitShutdownError indicates failure during storage engine flush or manifest sync during shutdown.
	ExitShutdownError = 4
)

// run parses arguments, configures the daemon, and executes the lifecycle loop.
func run(args []string, stdout, stderr io.Writer) int {
	return runWithContext(context.Background(), args, stdout, stderr, nil)
}

// runWithContext executes the daemon lifecycle with context and optional readiness signal.
func runWithContext(ctx context.Context, args []string, stdout, stderr io.Writer, readyCh chan<- struct{}) int {
	if len(args) > 0 && args[0] == "inspect-sstable" {
		return runInspectSSTable(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "dump-wal" {
		return runDumpWAL(args[1:], stdout, stderr)
	}

	cfg, isHelpOrVersion, err := ParseFlags(args, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "lattice: %v\n", err)
		return ExitConfigError
	}
	if isHelpOrVersion {
		return ExitSuccess
	}

	return runDaemon(ctx, cfg, stdout, stderr, readyCh)
}

// runDaemon coordinates the strict lifecycle of the Engine, Pprof Server, and Transport Server:
//
// Startup Order:
//  1. Validate configuration & loopback policy.
//  2. Open storage engine (directory initialization, crash recovery replay).
//  3. Initialize pprof diagnostics server (if enabled).
//  4. Construct and bind TCP transport server.
//  5. Start pprof HTTP server (if enabled).
//  6. Signal readiness and enter running state.
//
// Shutdown Order:
//  1. Signal received or context cancelled.
//  2. Transport Server.Shutdown drains active TCP connections within deadline.
//  3. Pprof Server.Shutdown drains diagnostics HTTP connections.
//  4. Engine.Close drains immutable memtables, writes L0 SSTable, and syncs WAL/MANIFEST.
//  5. Exit with success.
func runDaemon(ctx context.Context, cfg *Config, stdout, stderr io.Writer, readyCh chan<- struct{}) int {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Early abort check
	select {
	case <-ctx.Done():
		return ExitSuccess
	case <-sigCh:
		return ExitSuccess
	default:
	}

	// Step 1: Open Storage Engine
	eng := engine.NewEngineWithOptions(engine.EngineOptions{DBPath: cfg.DataDir})
	if err := eng.Open(); err != nil {
		fmt.Fprintf(stderr, "lattice: failed to open storage engine at %s: %v\n", cfg.DataDir, err)
		return ExitStartupError
	}

	// Startup Signal Race Check (after Engine.Open, before Server setup)
	select {
	case <-ctx.Done():
		_ = eng.Close()
		return ExitSuccess
	case sig := <-sigCh:
		fmt.Fprintf(stdout, "lattice: received signal %s during startup, closing engine...\n", sig)
		_ = eng.Close()
		return ExitSuccess
	default:
	}

	// Step 2: Initialize Pprof Server if configured
	var pprofSrv *PprofServer
	if cfg.PprofAddress != "" {
		var err error
		pprofSrv, err = NewPprofServer(cfg.PprofAddress)
		if err != nil {
			_ = eng.Close()
			fmt.Fprintf(stderr, "lattice: failed to start pprof listener on %s: %v\n", cfg.PprofAddress, err)
			return ExitStartupError
		}
	}

	// Step 3: Initialize Transport Server & Raft Subsystem (GAP C)
	srvCfg := transport.DefaultServerConfig()
	srvCfg.Address = cfg.Address
	srvCfg.InsecureTransport = cfg.InsecureTransport

	var (
		raftStorage *raft.Storage
		raftNode    *raft.Node
		peerMgr     *transport.PeerConnectionManager
	)

	// In cluster mode: wire persistent Raft storage, Node, ProposalRouter, and apply loop
	if cfg.IsClusterEnabled() {
		srvCfg.ClusterMode = true

		raftDir := filepath.Join(cfg.DataDir, "raft")
		if err := os.MkdirAll(raftDir, 0750); err != nil {
			if pprofSrv != nil {
				_ = pprofSrv.Shutdown(context.Background())
			}
			_ = eng.Close()
			fmt.Fprintf(stderr, "lattice: failed to create raft storage directory at %s: %v\n", raftDir, err)
			return ExitStartupError
		}

		var err error
		raftStorage, err = raft.OpenStorage(raftDir)
		if err != nil {
			if pprofSrv != nil {
				_ = pprofSrv.Shutdown(context.Background())
			}
			_ = eng.Close()
			fmt.Fprintf(stderr, "lattice: failed to open raft storage at %s: %v\n", raftDir, err)
			return ExitStartupError
		}

		peerCfg := transport.DefaultPeerConnectionConfig()
		peerCfg.InsecureTransport = cfg.InsecureTransport
		peerCfg.OnFrameReceived = func(peerID cluster.NodeID, frame *transport.Frame) {
			if raftNode != nil {
				_ = raftNode.HandlePeerFrame(peerID, frame)
			}
		}

		peerMgr, err = transport.NewPeerConnectionManager(cfg.Topology, peerCfg)
		if err != nil {
			_ = raftStorage.Close()
			if pprofSrv != nil {
				_ = pprofSrv.Shutdown(context.Background())
			}
			_ = eng.Close()
			fmt.Fprintf(stderr, "lattice: failed to initialize peer connection manager: %v\n", err)
			return ExitStartupError
		}

		raftCfg := raft.NodeConfig{
			LocalID:      cluster.NodeID(cfg.NodeID),
			Storage:      raftStorage,
			Topology:     cfg.Topology,
			StateMachine: eng,
			PeerSender:   peerMgr,
		}
		raftNode, err = raft.NewNode(raftCfg)
		if err != nil {
			_ = peerMgr.Close()
			_ = raftStorage.Close()
			if pprofSrv != nil {
				_ = pprofSrv.Shutdown(context.Background())
			}
			_ = eng.Close()
			fmt.Fprintf(stderr, "lattice: failed to initialize raft node: %v\n", err)
			return ExitStartupError
		}

		router := raft.NewProposalRouter(raftNode, cfg.Topology, eng)
		srvCfg.ProposalRouter = router
		srvCfg.ReadRouter = router

		// Start peer listener if cluster topology has local address and remote peers exist
		if cfg.Topology != nil && cfg.Topology.LocalAddress() != "" && cfg.Topology.Size() > 1 {
			if err := peerMgr.StartListener(cfg.Topology.LocalAddress()); err != nil {
				_ = raftNode.Close()
				_ = peerMgr.Close()
				_ = raftStorage.Close()
				if pprofSrv != nil {
					_ = pprofSrv.Shutdown(context.Background())
				}
				_ = eng.Close()
				fmt.Fprintf(stderr, "lattice: failed to start peer listener on %s: %v\n", cfg.Topology.LocalAddress(), err)
				return ExitStartupError
			}
		}

		// Start peer connection supervisor loops
		if err := peerMgr.Start(); err != nil {
			_ = raftNode.Close()
			_ = peerMgr.Close()
			_ = raftStorage.Close()
			if pprofSrv != nil {
				_ = pprofSrv.Shutdown(context.Background())
			}
			_ = eng.Close()
			fmt.Fprintf(stderr, "lattice: failed to start peer connection manager: %v\n", err)
			return ExitStartupError
		}

		// Start election services
		if cfg.Topology != nil && cfg.Topology.Size() == 1 {
			// In single-node cluster (N=1), candidate self-vote satisfies quorum immediately
			if err := raftNode.BecomeCandidate(); err == nil {
				_ = raftNode.BecomeLeader()
			}
		}
		if err := raftNode.StartElectionTimer(); err != nil {
			_ = raftNode.Close()
			_ = peerMgr.Close()
			_ = raftStorage.Close()
			if pprofSrv != nil {
				_ = pprofSrv.Shutdown(context.Background())
			}
			_ = eng.Close()
			fmt.Fprintf(stderr, "lattice: failed to start election timer: %v\n", err)
			return ExitStartupError
		}
	}

	srv, err := transport.NewServer(srvCfg, eng)
	if err != nil {
		if peerMgr != nil {
			_ = peerMgr.Close()
		}
		if raftNode != nil {
			_ = raftNode.Close()
		}
		if raftStorage != nil {
			_ = raftStorage.Close()
		}
		if pprofSrv != nil {
			_ = pprofSrv.Shutdown(context.Background())
		}
		_ = eng.Close()
		fmt.Fprintf(stderr, "lattice: failed to initialize transport server: %v\n", err)
		return ExitStartupError
	}

	// Step 4: Bind Listener and Start Accept Loop
	if err := srv.Listen(cfg.Address); err != nil {
		if peerMgr != nil {
			_ = peerMgr.Close()
		}
		if raftNode != nil {
			_ = raftNode.Close()
		}
		if raftStorage != nil {
			_ = raftStorage.Close()
		}
		if pprofSrv != nil {
			_ = pprofSrv.Shutdown(context.Background())
		}
		_ = eng.Close()
		fmt.Fprintf(stderr, "lattice: failed to start listener on %s: %v\n", cfg.Address, err)
		return ExitStartupError
	}

	// Startup Signal Race Check (after Server.Listen)
	select {
	case <-ctx.Done():
		_ = srv.Close()
		if peerMgr != nil {
			_ = peerMgr.Close()
		}
		if raftNode != nil {
			_ = raftNode.Close()
		}
		if raftStorage != nil {
			_ = raftStorage.Close()
		}
		if pprofSrv != nil {
			_ = pprofSrv.Shutdown(context.Background())
		}
		_ = eng.Close()
		return ExitSuccess
	case sig := <-sigCh:
		fmt.Fprintf(stdout, "lattice: received signal %s during startup, shutting down...\n", sig)
		_ = srv.Close()
		if peerMgr != nil {
			_ = peerMgr.Close()
		}
		if raftNode != nil {
			_ = raftNode.Close()
		}
		if raftStorage != nil {
			_ = raftStorage.Close()
		}
		if pprofSrv != nil {
			_ = pprofSrv.Shutdown(context.Background())
		}
		_ = eng.Close()
		return ExitSuccess
	default:
	}

	// Step 5: Start Pprof Server if configured
	if pprofSrv != nil {
		if err := pprofSrv.Start(); err != nil {
			_ = srv.Close()
			if peerMgr != nil {
				_ = peerMgr.Close()
			}
			if raftNode != nil {
				_ = raftNode.Close()
			}
			if raftStorage != nil {
				_ = raftStorage.Close()
			}
			_ = pprofSrv.Shutdown(context.Background())
			_ = eng.Close()
			fmt.Fprintf(stderr, "lattice: failed to start pprof server: %v\n", err)
			return ExitStartupError
		}
		pprofAddr := pprofSrv.Addr().String()
		fmt.Fprintf(stdout, "lattice: pprof diagnostics listening on http://%s/debug/pprof/\n", pprofAddr)
	}

	// Step 6: Running State Established
	if cfg.Topology != nil {
		fmt.Fprintf(stdout, "lattice: cluster topology initialized (node_id: %d, peers: %d, endpoint: %s)\n",
			cfg.Topology.LocalID(), cfg.Topology.Size(), cfg.Topology.LocalAddress())
	}

	boundAddr := srv.Addr()
	addrStr := cfg.Address
	if boundAddr != nil {
		addrStr = boundAddr.String()
	}

	fmt.Fprintf(stdout, "lattice: server listening on %s (data-dir: %s)\n", addrStr, cfg.DataDir)
	if readyCh != nil {
		close(readyCh)
	}

	// Step 7: Wait for Termination Event
	select {
	case sig := <-sigCh:
		fmt.Fprintf(stdout, "lattice: received signal %s, initiating graceful shutdown...\n", sig)
	case <-ctx.Done():
		fmt.Fprintf(stdout, "lattice: context cancelled, initiating graceful shutdown...\n")
	}

	// Absorb any additional/repeated signals in the background so they do not trigger panic or re-entry
	go func() {
		for range sigCh {
			// Repeated signals are safely absorbed during shutdown
		}
	}()

	// Step 8: Ordered Graceful Shutdown
	// Invariant: Halt network ingestion and drain data clients BEFORE Raft/pprof/storage,
	// stop Raft node and apply loop, close Raft persistent storage,
	// drain pprof diagnostics, and close storage engine LAST.
	shutCtx, cancel := context.WithTimeout(context.Background(), srvCfg.ShutdownTimeout)
	defer cancel()

	if srvErr := srv.Shutdown(shutCtx); srvErr != nil {
		fmt.Fprintf(stderr, "lattice: warning: server network shutdown error: %v\n", srvErr)
	}

	if peerMgr != nil {
		_ = peerMgr.Close()
	}

	if raftNode != nil {
		_ = raftNode.Close()
	}

	if raftStorage != nil {
		if rErr := raftStorage.Close(); rErr != nil {
			fmt.Fprintf(stderr, "lattice: warning: raft storage close error: %v\n", rErr)
		}
	}

	if pprofSrv != nil {
		if pprofErr := pprofSrv.Shutdown(shutCtx); pprofErr != nil {
			fmt.Fprintf(stderr, "lattice: warning: pprof server shutdown error: %v\n", pprofErr)
		} else if sErr := pprofSrv.Err(); sErr != nil {
			fmt.Fprintf(stderr, "lattice: warning: pprof server accept error: %v\n", sErr)
		}
	}

	if engErr := eng.Close(); engErr != nil {
		fmt.Fprintf(stderr, "lattice: error: storage engine close failure: %v\n", engErr)
		return ExitShutdownError
	}

	fmt.Fprintf(stdout, "lattice: shutdown complete, all storage artifacts synced\n")
	return ExitSuccess
}
