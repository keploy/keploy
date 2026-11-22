//go:build windows

package mock

import "testing"

// mkfifo is unreachable on windows: every caller skips first.
func mkfifo(t *testing.T, _ string) { t.Skip("no FIFOs on windows") }
