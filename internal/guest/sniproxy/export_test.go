package sniproxy

import "testing"

// ECHHello returns a minimal ClientHello record naming serverName that also
// carries an encrypted_client_hello extension, as Chromium's ECH-GREASE does.
func ECHHello(t *testing.T, serverName string) []byte {
	t.Helper()
	exts := append(sniExt(serverNameTypeHostName, serverName), ext(extensionEncryptedClientHello, []byte{0x00})...)
	return handBuiltHello(t, exts, -1)
}
