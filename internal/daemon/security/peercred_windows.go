//go:build windows

package security

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// VerifyPeerCredentials validates peer connection security on Windows.
func VerifyPeerCredentials(conn net.Conn) error {
	// On Windows, AF_UNIX does not support SO_PEERCRED.
	// Security relies on directory DACLs and ephemeral auth token handshakes.
	return nil
}

// GenerateAuthToken creates and writes a cryptographically secure ephemeral token.
func GenerateAuthToken(tokenPath string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0700); err != nil {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		return "", fmt.Errorf("failed to write auth token: %w", err)
	}
	return token, nil
}
