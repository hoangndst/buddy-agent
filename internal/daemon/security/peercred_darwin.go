//go:build darwin

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

	var xucred *unix.Xucred
	var credErr error
	err = rawConn.Control(func(fd uintptr) {
		xucred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	})
	if err != nil {
		return fmt.Errorf("control error reading peer credentials: %w", err)
	}
	if credErr != nil {
		return fmt.Errorf("failed to read LOCAL_PEERCRED: %w", credErr)
	}

	expectedUID := uint32(os.Getuid())
	if xucred.Uid != expectedUID {
		return fmt.Errorf("peer UID %d does not match daemon UID %d; rejecting", xucred.Uid, expectedUID)
	}
	return nil
}
