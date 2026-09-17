package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/silent-knight19/lattice/internal/engine"
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

// runDaemon coordinates the strict lifecycle of the Engine and Transport Server:
//
// Startup Order:
//  1. Validate configuration & loopback policy.
//  2. Open storage engine (directory initialization, crash recovery replay).
//  3. Construct and bind TCP transport server.
//  4. Signal readiness and enter running state.
//
// Shutdown Order:
//  1. Signal received or context cancelled.
//  2. Server.Shutdown drains active TCP connections within deadline.
//  3. Engine.Close drains immutable memtables, writes L0 SSTable, and syncs WAL/MANIFEST.
//  4. Exit with success.
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

	// Step 2: Initialize Transport Server
	srvCfg := transport.DefaultServerConfig()
	srvCfg.Address = cfg.Address
	srvCfg.InsecureTransport = cfg.InsecureTransport

	srv, err := transport.NewServer(srvCfg, eng)
	if err != nil {
		_ = eng.Close()
		fmt.Fprintf(stderr, "lattice: failed to initialize transport server: %v\n", err)
		return ExitStartupError
	}

	// Step 3: Bind Listener and Start Accept Loop
	if err := srv.Listen(cfg.Address); err != nil {
		_ = eng.Close()
		fmt.Fprintf(stderr, "lattice: failed to start listener on %s: %v\n", cfg.Address, err)
		return ExitStartupError
	}

	// Startup Signal Race Check (after Server.Listen)
	select {
	case <-ctx.Done():
		_ = srv.Close()
		_ = eng.Close()
		return ExitSuccess
	case sig := <-sigCh:
		fmt.Fprintf(stdout, "lattice: received signal %s during startup, shutting down...\n", sig)
		_ = srv.Close()
		_ = eng.Close()
		return ExitSuccess
	default:
	}

	// Step 4: Running State Established
	boundAddr := srv.Addr()
	addrStr := cfg.Address
	if boundAddr != nil {
		addrStr = boundAddr.String()
	}

	fmt.Fprintf(stdout, "lattice: server listening on %s (data-dir: %s)\n", addrStr, cfg.DataDir)
	if readyCh != nil {
		close(readyCh)
	}

	// Step 5: Wait for Termination Event
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

	// Step 6: Ordered Graceful Shutdown
	// Invariant: Halt network ingestion and drain clients BEFORE closing the storage engine.
	shutCtx, cancel := context.WithTimeout(context.Background(), srvCfg.ShutdownTimeout)
	defer cancel()

	if srvErr := srv.Shutdown(shutCtx); srvErr != nil {
		fmt.Fprintf(stderr, "lattice: warning: server network shutdown error: %v\n", srvErr)
	}

	if engErr := eng.Close(); engErr != nil {
		fmt.Fprintf(stderr, "lattice: error: storage engine close failure: %v\n", engErr)
		return ExitShutdownError
	}

	fmt.Fprintf(stdout, "lattice: shutdown complete, all storage artifacts synced\n")
	return ExitSuccess
}
