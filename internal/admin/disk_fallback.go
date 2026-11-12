//go:build !darwin && !linux

package admin

func getDiskSpace(path string) (free uint64, total uint64) {
	return 50 * 1024 * 1024 * 1024, 100 * 1024 * 1024 * 1024
}
