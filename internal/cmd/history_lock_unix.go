//go:build !windows

package cmd

import (
	"os"

	"golang.org/x/sys/unix"
)

func acquireHistoryFileLock(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_EX) }
func releaseHistoryFileLock(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }
