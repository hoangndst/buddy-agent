//go:build !linux && !darwin && !windows

package security

import "net"

// VerifyPeerCredentials fallback for other platforms.
func VerifyPeerCredentials(conn net.Conn) error {
	return nil
}
