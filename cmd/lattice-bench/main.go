package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const (
	// ExitSuccess indicates normal completion.
	ExitSuccess = 0

	// ExitConfigError indicates invalid command line arguments or configuration bounds violation.
	ExitConfigError = 1

	// ExitRuntimeError indicates network connection failure or catastrophic execution error.
	ExitRuntimeError = 2
)

func main() {
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
}

// run parses flags, wires signal cancellation, executes the benchmark, and prints the report.
func run(args []string, stdout, stderr io.Writer) int {
	cfg, isHelpOrVersion, err := ParseFlags(args, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "lattice-bench: %v\n", err)
		return ExitConfigError
	}
	if isHelpOrVersion {
		return ExitSuccess
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	go func() {
		select {
		case sig := <-sigCh:
			fmt.Fprintf(stderr, "\nlattice-bench: received %s, initiating graceful shutdown...\n", sig)
			cancel()
		case <-ctx.Done():
		}
	}()

	res, err := Run(ctx, cfg, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "lattice-bench: error: %v\n", err)
		return ExitRuntimeError
	}

	report := FormatReport(res)
	fmt.Fprint(stdout, report)

	// If no operations succeeded due to complete server failure, exit with runtime error
	if res.TotalOps == 0 && (res.GetErrors > 0 || res.PutErrors > 0 || res.NetErrors > 0) {
		fmt.Fprintf(stderr, "lattice-bench: benchmark completed with 0 successful operations\n")
		return ExitRuntimeError
	}

	return ExitSuccess
}
