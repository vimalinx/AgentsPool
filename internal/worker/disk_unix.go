//go:build !windows

package worker

import "syscall"

func diskUsage(path string) (totalMB, availableMB int64, err error) {
	var stat syscall.Statfs_t
	if err = syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	blockSize := int64(stat.Bsize)
	totalMB = int64(stat.Blocks) * blockSize / (1024 * 1024)
	availableMB = int64(stat.Bavail) * blockSize / (1024 * 1024)
	return totalMB, availableMB, nil
}
