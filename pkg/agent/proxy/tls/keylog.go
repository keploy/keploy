package tls

import (
	"io"
	"sync"
)

// keyLog is the package-level NSS-format key log writer. It is set by
// the proxy when --capture-packets is on so every tls.Config the proxy
// builds (client-side MITM and upstream re-dial) can dump session
// secrets into a single sslkeys.log next to the test-set's pcap.
//
// Wireshark consumes the resulting file via Edit → Preferences →
// Protocols → TLS → "(Pre)-Master-Secret log filename", which lets it
// decrypt the encrypted frames already captured in traffic.pcap.
//
// The writer is replaced atomically per recording session: each
// Record() call points it at the new test-set's file, each Mock() /
// SetGracefulShutdown() clears it (alongside stopPacketCapture).
//
// Treat the keylog file as sensitive — anyone with the pcap and the
// log can read every TLS session in plaintext.
var (
	keyLogMu sync.RWMutex
	keyLog   io.Writer
)

// SetKeyLogWriter installs w as the package-wide writer that future
// tls.Config builders read via KeyLogWriter(). Pass nil to disable.
// Safe to call concurrently.
func SetKeyLogWriter(w io.Writer) {
	keyLogMu.Lock()
	keyLog = w
	keyLogMu.Unlock()
}

// KeyLogWriter returns the currently installed writer or nil when
// capture is off. Callers should plumb the result into
// tls.Config.KeyLogWriter unconditionally — stdlib treats a nil writer
// as "no key log".
func KeyLogWriter() io.Writer {
	keyLogMu.RLock()
	w := keyLog
	keyLogMu.RUnlock()
	return w
}
