//go:build unix

package config

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes an exclusive flock on f without blocking. busy is true when
// another open file description holds it, which includes another goroutine of
// this process that opened the lock file separately: flock locks belong to the
// open file, not to the process, so the in-process case is covered too.
func tryLock(f *os.File) (busy bool, err error) {
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) //nolint:gosec // G115: a file descriptor always fits an int
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

func unlock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN) //nolint:gosec // G115: as above
}

// syncDir makes a rename in dir durable: until the directory itself is
// flushed, a crash can bring back the old entry after the new file's bytes
// were already on disk. Best effort, because some filesystems refuse fsync on
// a directory, and the rename has happened either way.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // G304: the config directory itself
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
