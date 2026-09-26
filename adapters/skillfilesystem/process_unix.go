//go:build !windows

package skillfilesystem

import (
	"errors"
	"syscall"
)

func defaultProcessState(pid int) processState {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return processAlive
	case errors.Is(err, syscall.ESRCH):
		return processDead
	default:
		return processUnknown
	}
}
