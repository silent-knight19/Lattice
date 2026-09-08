package dangerous

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"unsafe"
)

// VulnerableUnsafe demonstrates forbidden unsafe package usage.
func VulnerableUnsafe() {
	var x int = 42
	ptr := unsafe.Pointer(&x)
	_ = ptr
}

// VulnerableExec demonstrates forbidden external subprocess execution.
func VulnerableExec(cmdName string) {
	cmd := exec.Command(cmdName)
	_ = cmd.Run()
}

// VulnerablePerms demonstrates dangerous world-writable file creation.
func VulnerablePerms(path string) {
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0777)
	if f != nil {
		_ = f.Close()
	}
}

// VulnerableRandom demonstrates weak randomness in production code.
func VulnerableRandom() int {
	return rand.Intn(100)
}

// DecodePacketVulnerable demonstrates forbidden panic on untrusted input decoding.
func DecodePacketVulnerable(data []byte) {
	if len(data) == 0 {
		panic("invalid empty packet")
	}
}

// ReadStreamUnbounded demonstrates forbidden unbounded allocation from untrusted length.
func ReadStreamUnbounded(r io.Reader, userSuppliedLen int) []byte {
	buf := make([]byte, userSuppliedLen)
	_, _ = io.ReadFull(r, buf)
	return buf
}

// HardcodedSecretExample demonstrates synthetic hardcoded credential detection.
func HardcodedSecretExample() string {
	apiKey := "synthetic_token_abcdef1234567890"
	return fmt.Sprintf("key=%s", apiKey)
}
