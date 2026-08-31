//go:build !windows

package storage

import "syscall"

func defaultProcessAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}
