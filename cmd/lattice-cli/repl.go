package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/silent-knight19/lattice/internal/transport"
)

const (
	// ExitSuccess indicates clean, successful execution or clean REPL session termination.
	ExitSuccess = 0

	// ExitConfigError indicates invalid command line flags or parameters.
	ExitConfigError = 1

	// ExitNetworkError indicates TCP connection failure, transport error, or broken connection.
	ExitNetworkError = 2

	// ExitRuntimeError indicates unrecoverable I/O or terminal failure.
	ExitRuntimeError = 3
)

// Style provides optional ANSI color formatting when connected to a terminal.
type Style struct {
	enabled bool
}

// NewStyle creates a Style instance.
func NewStyle(enabled bool) *Style {
	return &Style{enabled: enabled}
}

func (s *Style) Green(msg string) string {
	if !s.enabled {
		return msg
	}
	return "\033[32m" + msg + "\033[0m"
}

func (s *Style) Red(msg string) string {
	if !s.enabled {
		return msg
	}
	return "\033[31m" + msg + "\033[0m"
}

func (s *Style) Yellow(msg string) string {
	if !s.enabled {
		return msg
	}
	return "\033[33m" + msg + "\033[0m"
}

func (s *Style) Cyan(msg string) string {
	if !s.enabled {
		return msg
	}
	return "\033[36m" + msg + "\033[0m"
}

func (s *Style) Bold(msg string) string {
	if !s.enabled {
		return msg
	}
	return "\033[1m" + msg + "\033[0m"
}

// isTerminal reports whether the provided stream is an interactive character device terminal.
func isTerminal(w any) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

// FormatValue formats a byte slice for terminal display, escaping non-printable
// or binary byte sequences and distinguishing empty values from missing keys.
func FormatValue(val []byte) string {
	if len(val) == 0 {
		return `""`
	}

	// Check if the value is printable UTF-8 text
	isPrintable := true
	for i := 0; i < len(val); {
		r, size := utf8.DecodeRune(val[i:])
		if r == utf8.RuneError && size == 1 {
			isPrintable = false
			break
		}
		if r == '\r' {
			// Bare carriage return without newline is treated as non-printable to prevent terminal line rewriting
			if i+1 >= len(val) || val[i+1] != '\n' {
				isPrintable = false
				break
			}
		} else if !strconv.IsPrint(r) && r != '\n' && r != '\t' {
			isPrintable = false
			break
		}
		i += size
	}

	if isPrintable {
		return string(val)
	}

	// Escape non-printable binary bytes with \xHH
	var sb strings.Builder
	sb.WriteByte('"')
	for _, b := range val {
		switch b {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			if b >= 0x20 && b < 0x7f {
				sb.WriteByte(b)
			} else {
				fmt.Fprintf(&sb, `\x%02x`, b)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// printHelp displays the REPL help screen.
func printHelp(out io.Writer, style *Style) {
	fmt.Fprintf(out, "%s\n\n", style.Bold("Lattice Interactive REPL Commands:"))
	fmt.Fprintf(out, "  %s  %s\n", style.Cyan("PUT <key> <value>"), "- Store a key-value pair (supports quotes and \\xHH escapes)")
	fmt.Fprintf(out, "  %s        %s\n", style.Cyan("GET <key>"), "- Retrieve a value by key")
	fmt.Fprintf(out, "  %s     %s\n", style.Cyan("DELETE <key>"), "- Delete a key")
	fmt.Fprintf(out, "  %s      %s\n", style.Cyan("BATCH <ops>"), "- Atomic write batch (e.g. BATCH PUT k1 v1 DELETE k2)")
	fmt.Fprintf(out, "  %s     %s\n", style.Cyan("EXISTS <key>"), "- Check if key exists")
	fmt.Fprintf(out, "  %s            %s\n", style.Cyan("STATS"), "- Query server statistics and diagnostic snapshot")
	fmt.Fprintf(out, "  %s             %s\n", style.Cyan("HELP"), "- Display this help documentation")
	fmt.Fprintf(out, "  %s       %s\n\n", style.Cyan("EXIT, QUIT"), "- Exit the REPL session")
	fmt.Fprintf(out, "%s\n", style.Bold("Examples:"))
	fmt.Fprintf(out, "  PUT user:1001 '{\"name\": \"Alice\", \"role\": \"admin\"}'\n")
	fmt.Fprintf(out, "  GET user:1001\n")
	fmt.Fprintf(out, "  PUT bin \"\\x00\\x01\\x02\\xff\"\n")
	fmt.Fprintf(out, "  PUT empty \"\"\n")
	fmt.Fprintf(out, "  DELETE user:1001\n")
	fmt.Fprintf(out, "  BATCH PUT k1 v1 PUT k2 v2 DELETE user:1001\n")
}

// readLineBounded reads up to maxBytes from r until newline, preventing memory exhaustion DoS.
func readLineBounded(r *bufio.Reader, maxBytes int) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			if len(line) > 0 && err == io.EOF {
				return line, nil
			}
			return nil, err
		}
		if len(line)+len(chunk) > maxBytes {
			// Discard the remainder of the overlong line so the next command starts fresh
			for isPrefix && err == nil {
				_, isPrefix, err = r.ReadLine()
			}
			return nil, fmt.Errorf("command line exceeds maximum allowed size (%d bytes)", maxBytes)
		}
		line = append(line, chunk...)
		if !isPrefix {
			break
		}
	}
	return line, nil
}

// executeCommand sends the parsed Command to the server via LatticeClient and renders the response.
func executeCommand(client LatticeClient, cmd *Command, out io.Writer, style *Style) error {
	switch cmd.Kind {
	case CmdPut:
		req := &transport.Request{
			OpCode: transport.OpPut,
			Key:    cmd.Key,
			Value:  cmd.Value,
		}
		resp, err := client.Execute(req)
		if err != nil {
			return err
		}
		if resp.Status == transport.StatusOk {
			fmt.Fprintln(out, style.Green("OK"))
		} else {
			fmt.Fprintf(out, "%s\n", style.Red(fmt.Sprintf("(error) %s", resp.Message)))
		}

	case CmdGet:
		req := &transport.Request{
			OpCode: transport.OpGet,
			Key:    cmd.Key,
		}
		resp, err := client.Execute(req)
		if err != nil {
			return err
		}
		if resp.Status == transport.StatusOk {
			fmt.Fprintln(out, FormatValue(resp.Value))
		} else if resp.Status == transport.StatusKeyNotFound {
			fmt.Fprintln(out, style.Yellow("NOT FOUND"))
		} else {
			fmt.Fprintf(out, "%s\n", style.Red(fmt.Sprintf("(error) %s", resp.Message)))
		}

	case CmdDelete:
		req := &transport.Request{
			OpCode: transport.OpDelete,
			Key:    cmd.Key,
		}
		resp, err := client.Execute(req)
		if err != nil {
			return err
		}
		if resp.Status == transport.StatusOk {
			fmt.Fprintln(out, style.Green("OK"))
		} else if resp.Status == transport.StatusKeyNotFound {
			fmt.Fprintln(out, style.Yellow("NOT FOUND"))
		} else {
			fmt.Fprintf(out, "%s\n", style.Red(fmt.Sprintf("(error) %s", resp.Message)))
		}

	case CmdExists:
		req := &transport.Request{
			OpCode: transport.OpExists,
			Key:    cmd.Key,
		}
		resp, err := client.Execute(req)
		if err != nil {
			return err
		}
		if resp.Status == transport.StatusOk {
			if resp.Exists {
				fmt.Fprintln(out, "true")
			} else {
				fmt.Fprintln(out, "false")
			}
		} else {
			fmt.Fprintf(out, "%s\n", style.Red(fmt.Sprintf("(error) %s", resp.Message)))
		}

	case CmdBatch:
		req := &transport.Request{
			OpCode: transport.OpBatch,
			Batch:  cmd.Batch,
		}
		resp, err := client.Execute(req)
		if err != nil {
			return err
		}
		if resp.Status == transport.StatusOk {
			fmt.Fprintln(out, style.Green("OK"))
		} else {
			fmt.Fprintf(out, "%s\n", style.Red(fmt.Sprintf("(error) %s", resp.Message)))
		}

	case CmdStats:
		req := &transport.Request{
			OpCode: transport.OpStats,
		}
		resp, err := client.Execute(req)
		if err != nil {
			return err
		}
		if resp.Status == transport.StatusOk {
			fmt.Fprintln(out, FormatValue(resp.Value))
		} else {
			fmt.Fprintf(out, "%s\n", style.Red(fmt.Sprintf("(error) %s", resp.Message)))
		}

	default:
		return fmt.Errorf("unhandled command kind: %v", cmd.Kind)
	}

	return nil
}

// runREPL runs the interactive or scripted command loop against the provided client.
func runREPL(ctx context.Context, client LatticeClient, in io.Reader, out, errOut io.Writer, isInteractive bool, style *Style) int {
	reader := bufio.NewReader(in)

	for {
		// Check for cancellation
		select {
		case <-ctx.Done():
			return ExitSuccess
		default:
		}

		if isInteractive {
			fmt.Fprint(out, style.Cyan("lattice> "))
		}

		rawLine, err := readLineBounded(reader, MaxInputLineSize)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if isInteractive {
					fmt.Fprintln(out)
				}
				return ExitSuccess
			}
			fmt.Fprintf(errOut, "error: %v\n", err)
			if isInteractive {
				continue
			}
			return ExitRuntimeError
		}

		line := string(bytes.TrimRight(rawLine, "\r\n"))
		if strings.TrimSpace(line) == "" {
			continue
		}

		cmd, err := ParseCommand(line)
		if err != nil {
			fmt.Fprintf(errOut, "error: %v\n", err)
			continue
		}
		if cmd == nil {
			continue
		}

		if cmd.Kind == CmdHelp {
			printHelp(out, style)
			continue
		}

		if cmd.Kind == CmdExit || cmd.Kind == CmdQuit {
			return ExitSuccess
		}

		if err := executeCommand(client, cmd, out, style); err != nil {
			fmt.Fprintf(errOut, "error: %v\n", err)
			return ExitNetworkError
		}
	}
}

// run handles configuration parsing, connection setup, and REPL lifecycle execution.
func run(args []string, in io.Reader, out, errOut io.Writer) int {
	return runWithContext(context.Background(), args, in, out, errOut)
}

// runWithContext executes the client with context cancellation and signal handling.
func runWithContext(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	cfg, isHelpOrVersion, err := ParseCLIFlags(args, out, errOut)
	if err != nil {
		fmt.Fprintf(errOut, "lattice-cli: %v\n", err)
		return ExitConfigError
	}
	if isHelpOrVersion {
		return ExitSuccess
	}

	// Determine terminal color support
	colorEnabled := !cfg.NoColor && os.Getenv("NO_COLOR") == "" && isTerminal(out)
	style := NewStyle(colorEnabled)

	interactive := isTerminal(in) && isTerminal(out)

	// Set up graceful interrupt handling
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Connect to running Lattice node
	client, err := DialConfig(cfg)
	if err != nil {
		fmt.Fprintf(errOut, "error: connection to %s failed: %v\n", cfg.Address, err)
		return ExitNetworkError
	}
	defer client.Close()

	if interactive {
		fmt.Fprintf(out, "Connected to Lattice server at %s (%s)\n", cfg.Address, Version)
		fmt.Fprintln(out, "Type 'help' for available commands, 'exit' or Ctrl-D to quit.")
	}

	return runREPL(ctx, client, in, out, errOut, interactive, style)
}
