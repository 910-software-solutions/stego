//go:build !linux

package audit

import "time"

// threadCPU has no count of the CPU time of one thread on this system: the cost tests then compare only the
// allocations (review of PR #391, L2).
func threadCPU(f func()) (time.Duration, bool) {
	f()
	return 0, false
}
