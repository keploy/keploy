//go:build !windows

package token

import (
	"io/fs"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestIsPrivate_ADirectoryAnotherUserOwnsIsNotPrivate covers the half of the
// check that TestWriteFile_DoesNotWriteIntoADirectoryAnotherUserOwns can only
// reach as root. Another user who takes the directory's name after a temp
// cleaner removed it can make their directory 0700 just as easily, so the mode
// alone proves nothing: only the owner tells it apart, and without that check
// WriteFile would hand them the token.
func TestIsPrivate_ADirectoryAnotherUserOwnsIsNotPrivate(t *testing.T) {
	me := uint32(os.Geteuid())
	for _, tc := range []struct {
		name string
		mode fs.FileMode
		uid  uint32
		want bool
	}{
		{name: "0700 and this user's", mode: 0o700, uid: me, want: true},
		{name: "0700 and another user's", mode: 0o700, uid: me + 1},
		{name: "this user's but open to others", mode: 0o755, uid: me},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fi := dirInfo{mode: fs.ModeDir | tc.mode, stat: &syscall.Stat_t{Uid: tc.uid}}
			require.Equal(t, tc.want, isPrivate(fi))
		})
	}
}

// dirInfo is the os.FileInfo of a directory with a given mode and owner, which
// only root could otherwise produce for another user.
type dirInfo struct {
	mode fs.FileMode
	stat *syscall.Stat_t
}

func (d dirInfo) Name() string       { return "keploy-agent-test" }
func (d dirInfo) Size() int64        { return 0 }
func (d dirInfo) Mode() fs.FileMode  { return d.mode }
func (d dirInfo) ModTime() time.Time { return time.Time{} }
func (d dirInfo) IsDir() bool        { return d.mode.IsDir() }
func (d dirInfo) Sys() any           { return d.stat }
