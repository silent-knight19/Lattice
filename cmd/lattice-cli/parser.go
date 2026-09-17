package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/silent-knight19/lattice/internal/transport"
)

const (
	// MaxInputLineSize bounds user command input to 5 MiB to prevent memory exhaustion DoS.
	MaxInputLineSize = 5 * 1024 * 1024
)

// CommandKind identifies the parsed REPL command.
type CommandKind int

const (
	CmdUnknown CommandKind = iota
	CmdPut
	CmdGet
	CmdDelete
	CmdExists
	CmdStats
	CmdHelp
	CmdExit
	CmdQuit
)

func (k CommandKind) String() string {
	switch k {
	case CmdPut:
		return "PUT"
	case CmdGet:
		return "GET"
	case CmdDelete:
		return "DELETE"
	case CmdExists:
		return "EXISTS"
	case CmdStats:
		return "STATS"
	case CmdHelp:
		return "HELP"
	case CmdExit:
		return "EXIT"
	case CmdQuit:
		return "QUIT"
	default:
		return "UNKNOWN"
	}
}

// Command represents a validated client command ready for execution.
type Command struct {
	Kind  CommandKind
	Key   []byte
	Value []byte
	Args  []string
}

// Tokenize splits an input line into byte tokens, respecting single quotes (verbatim)
// and double quotes (escapes like \n, \r, \t, \", \\, and \xHH).
func Tokenize(line string) ([][]byte, error) {
	if len(line) > MaxInputLineSize {
		return nil, fmt.Errorf("command line exceeds maximum allowed size (%d bytes)", MaxInputLineSize)
	}

	var tokens [][]byte
	input := []byte(line)
	n := len(input)
	i := 0

	for i < n {
		// Skip whitespace
		for i < n && (input[i] == ' ' || input[i] == '\t' || input[i] == '\r' || input[i] == '\n') {
			i++
		}
		if i >= n {
			break
		}

		var token bytes.Buffer
		hasToken := false

		for i < n {
			c := input[i]
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				break
			}

			if c == '\'' {
				// Single-quoted string: verbatim bytes until next single quote
				hasToken = true
				i++
				closed := false
				for i < n {
					if input[i] == '\'' {
						closed = true
						i++
						break
					}
					token.WriteByte(input[i])
					i++
				}
				if !closed {
					return nil, errors.New("unclosed single quote in input")
				}
			} else if c == '"' {
				// Double-quoted string: processes escape sequences
				hasToken = true
				i++
				closed := false
				for i < n {
					if input[i] == '"' {
						closed = true
						i++
						break
					}
					if input[i] == '\\' {
						i++
						if i >= n {
							return nil, errors.New("incomplete escape sequence at end of input")
						}
						esc := input[i]
						switch esc {
						case '\\':
							token.WriteByte('\\')
						case '"':
							token.WriteByte('"')
						case '\'':
							token.WriteByte('\'')
						case 'n':
							token.WriteByte('\n')
						case 'r':
							token.WriteByte('\r')
						case 't':
							token.WriteByte('\t')
						case 'x', 'X':
							// Hex escape \xHH
							if i+2 >= n {
								return nil, errors.New("incomplete hex escape sequence: requires 2 hex digits (e.g. \\x00)")
							}
							hexBytes := input[i+1 : i+3]
							decoded := make([]byte, 1)
							_, err := hex.Decode(decoded, hexBytes)
							if err != nil {
								return nil, fmt.Errorf("invalid hex escape sequence \\x%s: %w", string(hexBytes), err)
							}
							token.WriteByte(decoded[0])
							i += 2
						default:
							// Preserve literal character for other escapes
							token.WriteByte(esc)
						}
						i++
					} else {
						token.WriteByte(input[i])
						i++
					}
				}
				if !closed {
					return nil, errors.New("unclosed double quote in input")
				}
			} else {
				hasToken = true
				token.WriteByte(c)
				i++
			}
		}

		if hasToken {
			tokens = append(tokens, token.Bytes())
		}
	}

	return tokens, nil
}

// ParseCommand parses a line of text into a typed Command.
// Returns (nil, nil) for empty or whitespace-only lines.
func ParseCommand(line string) (*Command, error) {
	tokens, err := Tokenize(line)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, nil
	}

	cmdName := strings.ToUpper(string(tokens[0]))

	switch cmdName {
	case "PUT":
		if len(tokens) < 3 {
			return nil, errors.New("PUT requires key and value: PUT <key> <value>")
		}
		if len(tokens) > 3 {
			return nil, errors.New("PUT takes exactly 2 arguments: PUT <key> <value> (use quotes for values with spaces)")
		}
		key := tokens[1]
		val := tokens[2]
		if len(key) == 0 {
			return nil, errors.New("PUT key cannot be empty")
		}
		if len(key) > transport.MaxKeyLength {
			return nil, fmt.Errorf("PUT key length %d exceeds maximum %d", len(key), transport.MaxKeyLength)
		}
		if len(val) > transport.MaxValueLength {
			return nil, fmt.Errorf("PUT value length %d exceeds maximum %d", len(val), transport.MaxValueLength)
		}
		return &Command{
			Kind:  CmdPut,
			Key:   key,
			Value: val,
		}, nil

	case "GET":
		if len(tokens) < 2 {
			return nil, errors.New("GET requires a key: GET <key>")
		}
		if len(tokens) > 2 {
			return nil, errors.New("GET takes exactly 1 argument: GET <key>")
		}
		key := tokens[1]
		if len(key) == 0 {
			return nil, errors.New("GET key cannot be empty")
		}
		if len(key) > transport.MaxKeyLength {
			return nil, fmt.Errorf("GET key length %d exceeds maximum %d", len(key), transport.MaxKeyLength)
		}
		return &Command{
			Kind: CmdGet,
			Key:  key,
		}, nil

	case "DELETE":
		if len(tokens) < 2 {
			return nil, errors.New("DELETE requires a key: DELETE <key>")
		}
		if len(tokens) > 2 {
			return nil, errors.New("DELETE takes exactly 1 argument: DELETE <key>")
		}
		key := tokens[1]
		if len(key) == 0 {
			return nil, errors.New("DELETE key cannot be empty")
		}
		if len(key) > transport.MaxKeyLength {
			return nil, fmt.Errorf("DELETE key length %d exceeds maximum %d", len(key), transport.MaxKeyLength)
		}
		return &Command{
			Kind: CmdDelete,
			Key:  key,
		}, nil

	case "EXISTS":
		if len(tokens) < 2 {
			return nil, errors.New("EXISTS requires a key: EXISTS <key>")
		}
		if len(tokens) > 2 {
			return nil, errors.New("EXISTS takes exactly 1 argument: EXISTS <key>")
		}
		key := tokens[1]
		if len(key) == 0 {
			return nil, errors.New("EXISTS key cannot be empty")
		}
		if len(key) > transport.MaxKeyLength {
			return nil, fmt.Errorf("EXISTS key length %d exceeds maximum %d", len(key), transport.MaxKeyLength)
		}
		return &Command{
			Kind: CmdExists,
			Key:  key,
		}, nil

	case "STATS":
		if len(tokens) > 1 {
			return nil, errors.New("STATS takes no arguments: STATS")
		}
		return &Command{
			Kind: CmdStats,
		}, nil

	case "HELP":
		var args []string
		for _, tok := range tokens[1:] {
			args = append(args, string(tok))
		}
		return &Command{
			Kind: CmdHelp,
			Args: args,
		}, nil

	case "EXIT":
		if len(tokens) > 1 {
			return nil, errors.New("EXIT takes no arguments")
		}
		return &Command{
			Kind: CmdExit,
		}, nil

	case "QUIT":
		if len(tokens) > 1 {
			return nil, errors.New("QUIT takes no arguments")
		}
		return &Command{
			Kind: CmdQuit,
		}, nil

	default:
		return nil, fmt.Errorf("unknown command %q (type 'help' for available commands)", tokens[0])
	}
}
