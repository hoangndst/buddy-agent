//go:build !windows

// Package daemon manages background agent lifecycle and Unix domain socket IPC.
package daemon

import (
	"os"

	"golang.org/x/sys/unix"
)

func acquireFileLock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func releaseFileLock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
