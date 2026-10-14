package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lockFileName is the lock every writer of the config takes. It is a
// separate file because config.json itself is replaced by a rename on every
// save, and a lock held on an inode that has just been renamed away protects
// nothing.
const lockFileName = ".lock"

// lockTimeout bounds the wait for another billkit. A holder can legitimately
// sit inside a keychain call for keyringTimeout per key, so it is generous.
var lockTimeout = 60 * time.Second

const lockPoll = 20 * time.Millisecond

// ErrSkipSave, returned by an Update callback, ends the update without
// writing anything; Update then returns nil.
var ErrSkipSave = errors.New("config: skip save")

type fileLock struct{ f *os.File }

// acquireLock takes the exclusive config lock, <Dir()>/.lock.
//
// Every load-modify-save and the pre-1.0 move run under it. Without it, two
// billkit processes started together (a shell prompt and a script, two
// terminals, a parallel CI step) each moved the old config, each put the live
// key into the keychain under a fresh random account, and each wrote a file
// naming its own: the last rename won and the other account was referenced by
// nothing, a live key left in the keychain forever. Two concurrent logins lost
// one profile and leaked its key the same way.
//
// The lock is advisory (flock on Unix, LockFileEx on Windows), both of which
// the kernel drops when the process dies, so a crash never leaves a stale lock
// behind. When the config directory cannot even be created (a root-owned
// ~/.config) and a pre-1.0 config is still at ~/.billkit, the lock is taken
// there instead, since that is the only file such a run can write.
func acquireLock() (*fileLock, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	if mkErr := os.MkdirAll(dir, dirMode); mkErr != nil {
		path, pathErr := Path()
		old, ok := legacyCandidate(path)
		if pathErr != nil || !ok {
			return nil, mkErr
		}
		dir = filepath.Dir(old)
	}
	return lockIn(dir)
}

func lockIn(dir string) (*fileLock, error) {
	path := filepath.Join(dir, lockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, fileMode) //nolint:gosec // G304: the CLI's own lock file
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockTimeout)
	for {
		busy, err := tryLock(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("could not lock %s: %w", path, err)
		}
		if !busy {
			return &fileLock{f: f}, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("another billkit process has held %s for over %s; try again once it has finished", path, lockTimeout)
		}
		time.Sleep(lockPoll)
	}
}

func (l *fileLock) release() {
	_ = unlock(l.f)
	_ = l.f.Close()
}

// Update is the only safe way to change the stored config: it takes the lock,
// loads (moving a pre-1.0 config first if one is waiting), refuses a config a
// newer billkit wrote before fn can put anything in the keychain, runs fn, and
// saves. The lock is held from the read to the write, so a concurrent login
// can neither be overwritten nor overwrite this one.
//
// fn must not call Save itself (a Save inside fn is the same save, not a
// second one). An error from fn discards fn's changes and deletes any
// keychain entry it created; ErrSkipSave does the same and returns nil.
func Update(fn func(*Config) error) error {
	path, err := Path()
	if err != nil {
		return err
	}
	lock, err := acquireLock()
	if err != nil {
		return err
	}
	defer lock.release()
	cfg, err := loadLocked(path)
	if err != nil {
		return err
	}
	if err := cfg.checkWritable(); err != nil {
		return err
	}
	cfg.locked = true
	defer func() { cfg.locked = false }()
	if err := fn(cfg); err != nil {
		cfg.rollback()
		if errors.Is(err, ErrSkipSave) {
			return nil
		}
		return err
	}
	return cfg.saveLocked(path)
}

// loadLocked is Load's body, for a caller that holds the lock.
func loadLocked(path string) (*Config, error) {
	if cfg, moved := migrateLegacy(path); moved {
		return cfg, nil
	}
	finishInterruptedMove(path)
	return loadFrom(path)
}

// LoadReadOnly reads the config without taking the lock, moving anything, or
// touching the keychain: the pre-1.0 file is read where it is when the new
// one does not exist yet. It is for shell completion, which runs on every Tab
// press and must never be the thing that moves a user's keys.
func LoadReadOnly() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
		if old, ok := legacyCandidate(path); ok {
			return loadLegacy(old)
		}
	}
	return loadFrom(path)
}
