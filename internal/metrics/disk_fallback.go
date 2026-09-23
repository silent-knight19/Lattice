//go:build !unix && !darwin && !linux && !windows

package metrics

import (
	"errors"
)

func platformStatfs(path string) (uint64, uint64, error) {
	return 0, 0, errors.New("platform statfs not supported")
}
