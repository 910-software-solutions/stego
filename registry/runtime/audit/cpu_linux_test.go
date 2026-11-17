//go:build linux

package audit

import (
	"runtime"
	"syscall"
	"time"
)

// threadCPU runs f on one locked thread and gives the CPU time that the thread used, or false where the system has no
// count for one thread. Other processes and other goroutines do not change it, as they change the wall clock.
func threadCPU(f func()) (time.Duration, bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const rusageThread = 1 // RUSAGE_THREAD of Linux
	var before, after syscall.Rusage
	if syscall.Getrusage(rusageThread, &before) != nil {
		return 0, false
	}
	f()
	if syscall.Getrusage(rusageThread, &after) != nil {
		return 0, false
	}
	used := func(r syscall.Rusage) time.Duration {
		return time.Duration(r.Utime.Nano() + r.Stime.Nano())
	}
	return used(after) - used(before), true
}
