//go:build windows

package worker

func diskUsage(path string) (totalMB, availableMB int64, err error) {
	return 0, 0, nil
}
