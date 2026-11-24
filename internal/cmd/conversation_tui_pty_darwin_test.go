//go:build darwin

package cmd

import "golang.org/x/sys/unix"

func ptyInputIsRaw(fd int) (bool, error) {
	state, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return false, err
	}
	return state.Lflag&(unix.ICANON|unix.ECHO) == 0, nil
}
