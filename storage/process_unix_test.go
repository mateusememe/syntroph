//go:build !windows

package storage

import (
	"syscall"
	"testing"
)

func TestProcessLivenessTreatsPermissionDeniedAsAlive(t *testing.T) {
	if !processAliveResult(syscall.EPERM) {
		t.Fatal("EPERM must be treated as alive or unverifiable")
	}
	if processAliveResult(syscall.ESRCH) {
		t.Fatal("ESRCH must be treated as dead")
	}
}
