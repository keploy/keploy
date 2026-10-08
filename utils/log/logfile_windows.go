package log

import "os"

// Windows has neither O_NOFOLLOW nor O_NONBLOCK. A reparse point at the log's
// name is refused by the Lstat below instead, which leaves a window between
// the check and the open; creating a symlink needs a privilege or developer
// mode, which narrows who can use it.
const (
	oNoFollow = 0
	oNonBlock = 0
)

// openLogFile opens LogFileName in the working directory for this run to
// append to. A symlink there is refused -- nil, and why -- and the run logs to
// the console; a log that cannot be opened at all is an error, as it always
// was.
func openLogFile() (*os.File, string, error) {
	if fi, err := os.Lstat(LogFileName); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, "it is a symbolic link", nil
	}
	f, err := os.OpenFile(LogFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, logFileMode)
	if err != nil {
		return nil, "", err
	}
	return f, "", nil
}
