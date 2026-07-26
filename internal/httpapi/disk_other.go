//go:build !linux

package httpapi

import "errors"

// diskUsage is unavailable off Linux. The deployment target is Debian; this exists so
// that `go build` on a developer's laptop, whatever it runs, still works — /healthz
// reports the check as unavailable rather than failing.
func diskUsage(string) (total, free uint64, err error) {
	return 0, 0, errors.New("disk usage is only reported on Linux")
}
