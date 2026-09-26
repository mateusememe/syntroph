//go:build windows

package skillfilesystem

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8
	errorFileExists         = syscall.Errno(80)
	errorAlreadyExists      = syscall.Errno(183)
)

var moveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func publishLockCandidate(candidate, target string) (bool, error) {
	if err := moveFileEx(candidate, target, moveFileWriteThrough); err != nil {
		return false, err
	}
	return true, nil
}

func replaceFileAtomically(source, target string) error {
	return moveFileEx(source, target, moveFileReplaceExisting|moveFileWriteThrough)
}

func publishImmutableDirectory(source, target string) error {
	return moveFileEx(source, target, moveFileWriteThrough)
}

func moveFileEx(source, target string, flags uintptr) error {
	sourcePointer, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetPointer, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	result, _, callErr := moveFileExW.Call(
		uintptr(unsafe.Pointer(sourcePointer)),
		uintptr(unsafe.Pointer(targetPointer)),
		flags,
	)
	if result != 0 {
		return nil
	}
	if callErr == errorFileExists || callErr == errorAlreadyExists {
		return os.ErrExist
	}
	if callErr == syscall.Errno(0) {
		return syscall.EINVAL
	}
	return callErr
}

// Active file, directory, and lock publications use MoveFileExW with
// MOVEFILE_WRITE_THROUGH on Windows. There is no supported fsync-equivalent
// for directory handles, so the common post-publication barrier is a no-op.
func syncDirectory(string) error { return nil }
