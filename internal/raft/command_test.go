package raft

import (
	"bytes"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

func TestEncodeDecodeCommand_Put(t *testing.T) {
	tests := []struct {
		name  string
		key   []byte
		value []byte
	}{
		{
			name:  "standard key-value",
			key:   []byte("user:1001:profile"),
			value: []byte(`{"name":"alice","role":"admin"}`),
		},
		{
			name:  "empty value",
			key:   []byte("flag:active"),
			value: []byte{},
		},
		{
			name:  "nil value",
			key:   []byte("flag:nil"),
			value: nil,
		},
		{
			name:  "binary key and value",
			key:   []byte{0x00, 0x01, 0xfe, 0xff},
			value: []byte{0xde, 0xad, 0xbe, 0xef},
		},
		{
			name:  "max key size",
			key:   bytes.Repeat([]byte("k"), binary.MaxKeyLen),
			value: []byte("val"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			orig := Command{
				Op:    binary.OpTypePut,
				Key:   tc.key,
				Value: tc.value,
			}

			data, err := EncodeCommand(orig)
			if err != nil {
				t.Fatalf("EncodeCommand failed: %v", err)
			}

			decoded, err := DecodeCommand(data)
			if err != nil {
				t.Fatalf("DecodeCommand failed: %v", err)
			}

			if decoded.Op != orig.Op {
				t.Errorf("Op mismatch: got %v, want %v", decoded.Op, orig.Op)
			}
			if !bytes.Equal(decoded.Key, orig.Key) {
				t.Errorf("Key mismatch: got %q, want %q", decoded.Key, orig.Key)
			}

			expectedVal := orig.Value
			if expectedVal == nil {
				expectedVal = []byte{}
			}
			if !bytes.Equal(decoded.Value, expectedVal) {
				t.Errorf("Value mismatch: got %q, want %q", decoded.Value, expectedVal)
			}
		})
	}
}

func TestEncodeDecodeCommand_Delete(t *testing.T) {
	key := []byte("user:1001:profile")
	orig := Command{
		Op:    binary.OpTypeDelete,
		Key:   key,
		Value: nil,
	}

	data, err := EncodeCommand(orig)
	if err != nil {
		t.Fatalf("EncodeCommand failed: %v", err)
	}

	decoded, err := DecodeCommand(data)
	if err != nil {
		t.Fatalf("DecodeCommand failed: %v", err)
	}

	if decoded.Op != binary.OpTypeDelete {
		t.Errorf("Op mismatch: got %v, want Delete", decoded.Op)
	}
	if !bytes.Equal(decoded.Key, key) {
		t.Errorf("Key mismatch: got %q, want %q", decoded.Key, key)
	}
	if len(decoded.Value) != 0 {
		t.Errorf("Value for delete must be empty, got: %q", decoded.Value)
	}

	// Delete with non-empty value must be rejected during encode
	invalidDelete := Command{
		Op:    binary.OpTypeDelete,
		Key:   key,
		Value: []byte("should-not-exist"),
	}
	if _, err := EncodeCommand(invalidDelete); err == nil {
		t.Fatal("expected error encoding Delete command with non-empty value")
	}
}

func TestEncodeCommand_Validation(t *testing.T) {
	t.Run("empty key", func(t *testing.T) {
		cmd := Command{
			Op:    binary.OpTypePut,
			Key:   []byte{},
			Value: []byte("val"),
		}
		if _, err := EncodeCommand(cmd); err == nil {
			t.Fatal("expected error for empty key")
		}
	})

	t.Run("oversized key", func(t *testing.T) {
		cmd := Command{
			Op:    binary.OpTypePut,
			Key:   bytes.Repeat([]byte("k"), binary.MaxKeyLen+1),
			Value: []byte("val"),
		}
		if _, err := EncodeCommand(cmd); err == nil {
			t.Fatal("expected error for oversized key")
		}
	})

	t.Run("invalid op", func(t *testing.T) {
		cmd := Command{
			Op:    0x99,
			Key:   []byte("k"),
			Value: []byte("val"),
		}
		if _, err := EncodeCommand(cmd); err == nil {
			t.Fatal("expected error for invalid op")
		}
	})
}

func TestDecodeCommand_Malformed(t *testing.T) {
	t.Run("empty payload", func(t *testing.T) {
		_, err := DecodeCommand(nil)
		if err == nil {
			t.Fatal("expected error for nil payload")
		}
		_, err = DecodeCommand([]byte{})
		if err == nil {
			t.Fatal("expected error for empty payload")
		}
	})

	t.Run("truncated header", func(t *testing.T) {
		for size := 1; size < CommandHeaderSize; size++ {
			buf := make([]byte, size)
			if _, err := DecodeCommand(buf); err == nil {
				t.Fatalf("expected error for truncated header of size %d", size)
			}
		}
	})

	t.Run("invalid opcode in header", func(t *testing.T) {
		buf := make([]byte, CommandHeaderSize)
		buf[0] = 0x55 // unknown op
		if _, err := DecodeCommand(buf); err == nil {
			t.Fatal("expected error for unknown opcode")
		}
	})

	t.Run("delete opcode with trailing bytes", func(t *testing.T) {
		cmd := Command{
			Op:  binary.OpTypeDelete,
			Key: []byte("k"),
		}
		encoded, err := EncodeCommand(cmd)
		if err != nil {
			t.Fatal(err)
		}
		// Add trailing byte
		withTrailing := append(encoded, 0x01)
		if _, err := DecodeCommand(withTrailing); err == nil {
			t.Fatal("expected error for Delete with trailing bytes")
		}
	})

	t.Run("truncated body key", func(t *testing.T) {
		cmd := Command{
			Op:    binary.OpTypePut,
			Key:   []byte("my-key-is-long"),
			Value: []byte("my-value"),
		}
		encoded, err := EncodeCommand(cmd)
		if err != nil {
			t.Fatal(err)
		}
		// Truncate by 5 bytes
		truncated := encoded[:len(encoded)-5]
		if _, err := DecodeCommand(truncated); err == nil {
			t.Fatal("expected error for truncated body")
		}
	})

	t.Run("declared key length exceeds payload bounds", func(t *testing.T) {
		buf := make([]byte, CommandHeaderSize)
		buf[0] = byte(binary.OpTypePut)
		binary.PutUint16(buf[1:3], 5000)
		if _, err := DecodeCommand(buf); err == nil {
			t.Fatal("expected error when declared key size exceeds buffer")
		}
	})
}

func FuzzDecodeCommand(f *testing.F) {
	// Seed corpus with valid Put and Delete commands
	c1, _ := EncodeCommand(Command{Op: binary.OpTypePut, Key: []byte("key1"), Value: []byte("val1")})
	c2, _ := EncodeCommand(Command{Op: binary.OpTypeDelete, Key: []byte("key2")})
	c3, _ := EncodeCommand(Command{Op: binary.OpTypePut, Key: []byte("k"), Value: []byte{}})

	f.Add(c1)
	f.Add(c2)
	f.Add(c3)
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0x01, 0x00, 0x01, 'k'})

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic on arbitrary inputs
		cmd, err := DecodeCommand(data)
		if err != nil {
			return
		}
		// If decode succeeded, verify properties
		if cmd.Op != binary.OpTypePut && cmd.Op != binary.OpTypeDelete {
			t.Fatalf("unexpected valid op: %v", cmd.Op)
		}
		if len(cmd.Key) == 0 || len(cmd.Key) > binary.MaxKeyLen {
			t.Fatalf("invalid key length: %d", len(cmd.Key))
		}
		if cmd.Op == binary.OpTypeDelete && len(cmd.Value) != 0 {
			t.Fatalf("delete command has non-empty value: %d", len(cmd.Value))
		}
	})
}
