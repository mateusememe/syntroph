//go:build windows

package skillfilesystem

import "syscall"

const errorInvalidParameter syscall.Errno = 87

func defaultProcessState(pid int) processState {
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if err == errorInvalidParameter {
			return processDead
		}
		return processUnknown
	}
	defer syscall.CloseHandle(handle)
	status, err := syscall.WaitForSingleObject(handle, 0)
	if err != nil {
		return processUnknown
	}
	switch status {
	case syscall.WAIT_OBJECT_0:
		return processDead
	case syscall.WAIT_TIMEOUT:
		return processAlive
	default:
		return processUnknown
	}
}
