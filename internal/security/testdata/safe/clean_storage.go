package safe

import (
	"crypto/rand"
	"errors"
	"io"
	"os"
)

const (
	MaxSafeLen = 65536
)

// SafeStorageOperation demonstrates secure, bounded file and memory operations.
func SafeStorageOperation(path string, r io.Reader, lenVal int) ([]byte, error) {
	if lenVal > MaxSafeLen || lenVal <= 0 {
		return nil, errors.New("length exceeded")
	}

	buf := make([]byte, lenVal)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	return buf, nil
}
