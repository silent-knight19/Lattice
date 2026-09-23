//go:build unix || darwin || linux

package metrics

import (
	"syscall"
)

func platformStatfs(path string) (uint64, uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	totalBytes := stat.Blocks * uint64(stat.Bsize)
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	return totalBytes, freeBytes, nil
}
