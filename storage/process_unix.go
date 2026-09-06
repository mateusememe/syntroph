//go:build !windows

package storage

import (
	"errors"
	"syscall"
)

func defaultProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	// EPERM proves that a process occupies the PID even though the caller is
	// not allowed to signal it. Treating that as dead could clear a live lock.
	return processAliveResult(err)
}

func processAliveResult(err error) bool { return err == nil || errors.Is(err, syscall.EPERM) }
