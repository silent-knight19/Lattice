//go:build darwin || linux

package admin

import (
	"syscall"
)

func getDiskSpace(path string) (free uint64, total uint64) {
	if path == "" {
		path = "."
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 50 * 1024 * 1024 * 1024, 100 * 1024 * 1024 * 1024
	}
	bsize := uint64(stat.Bsize)
	free = uint64(stat.Bavail) * bsize
	total = uint64(stat.Blocks) * bsize
	return free, total
}
