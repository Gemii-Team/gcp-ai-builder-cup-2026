//go:build linux

package pdftext

import (
	"fmt"
	"os"
	"syscall"
)

const (
	// cpuSeconds is the CPU time a child may use. The parent's deadline is wall-clock
	// and comes first; this is for a child whose parent is gone.
	cpuSeconds = 30
	// addressSpace bounds the virtual memory of a child. The Go runtime reserves far
	// more address space than it uses, so this is a ceiling for a runaway allocation
	// (the library asks for a hundred gigabytes on a crafted file), not a measure of
	// the heap.
	addressSpace = 3 << 30
)

// confine limits what the child may use, before it touches the file. A limit that
// cannot be set is an error: a child that is not confined does not run.
func confine() error {
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{}); err != nil {
		return fmt.Errorf("core dumps: %w", err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: cpuSeconds, Max: cpuSeconds + 1}); err != nil {
		return fmt.Errorf("cpu time: %w", err)
	}
	// The race detector reserves terabytes of address space for its shadow memory, so a
	// test binary built with it cannot run under a limit; production binaries are not.
	if !raceBuild {
		if err := syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: addressSpace, Max: addressSpace}); err != nil {
			return fmt.Errorf("address space: %w", err)
		}
	}
	// If the host runs out of memory anyway, this is the process to take: the kernel
	// picks the victim by this score, and the server it serves must not be it. Not every
	// container lets a process write it, so a failure is ignored.
	_ = os.WriteFile("/proc/self/oom_score_adj", []byte("1000"), 0)
	return nil
}
