//go:build !unix && !windows

package config

import "os"

// tryLock always succeeds where the platform has no file lock to offer. No
// release targets such a platform; this keeps the package building on one.
func tryLock(*os.File) (busy bool, err error) { return false, nil }

func unlock(*os.File) error { return nil }

func syncDir(string) {}
