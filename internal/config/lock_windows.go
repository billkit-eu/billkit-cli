//go:build windows

package config

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes an exclusive LockFileEx lock on the first byte of f without
// blocking. Windows locks belong to the handle, so a second handle in this
// process is refused exactly like another process.
func tryLock(f *os.File) (busy bool, err error) {
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return true, nil
	}
	return false, err
}

func unlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

// syncDir is a no-op: Windows has no directory handle to flush, and
// MoveFileEx (os.Rename) is already as durable as the platform offers.
func syncDir(string) {}
