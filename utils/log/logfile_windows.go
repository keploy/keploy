package log

import (
	"fmt"
	"os"
)

// Windows has neither O_NOFOLLOW nor O_NONBLOCK. ReadLogFile's identity check
// (os.SameFile against the file this run created) still refuses whatever
// replaced the log at its name.
const (
	oNoFollow = 0
	oNonBlock = 0
)

// openClientLogFile opens path, the log the CLI that started this agent named
// (ClientLogFileFlag), to append to: only a regular file that already exists,
// no link, and the one that name was when it was checked. The agent runs as
// the CLI's user here (no sudo stands between them), so there is no owner to
// check.
func openClientLogFile(path string) (*os.File, error) {
	named, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("the CLI's log: %w", err)
	}
	if !named.Mode().IsRegular() {
		return nil, fmt.Errorf("the CLI's log %s is not a regular file (%v)", path, named.Mode().Type())
	}
	f, err := openFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return nil, fmt.Errorf("the CLI's log: %w", err)
	}
	opened, err := f.Stat()
	if err == nil && (!opened.Mode().IsRegular() || !os.SameFile(named, opened)) {
		err = fmt.Errorf("the CLI's log %s was replaced while it was opened", path)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
