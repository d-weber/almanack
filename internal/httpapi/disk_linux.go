//go:build linux

package httpapi

import (
	"fmt"
	"syscall"
)

// diskUsage reports the total and available bytes of the filesystem holding path.
//
// Bavail rather than Bfree: the reserved blocks only root may use are not space this
// service can write into, and reporting them free is how a "10% free" check passes on a
// disk the application can no longer write to.
func diskUsage(path string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	size := uint64(st.Bsize)
	return st.Blocks * size, st.Bavail * size, nil
}
