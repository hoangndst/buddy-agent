//go:build linux

// Package security provides peer credential verification across platforms.
package security

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// VerifyPeerCredentials verifies that the peer connecting over UDS belongs to the same UID.
func VerifyPeerCredentials(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("connection is not a unix socket")
	}

	rawConn, err := unixConn.SyscallConn()
	if err != nil {
		return fmt.Errorf("failed to get raw syscall conn: %w", err)
	}

	var cred *unix.Ucred
	var credErr error
	err = rawConn.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil {
		return fmt.Errorf("control error reading peer credentials: %w", err)
	}
	if credErr != nil {
		return fmt.Errorf("failed to read SO_PEERCRED: %w", credErr)
	}

	expectedUID := uint32(os.Getuid())
	if cred.Uid != expectedUID {
		return fmt.Errorf("peer UID %d does not match daemon UID %d; rejecting", cred.Uid, expectedUID)
	}
	return nil
}
