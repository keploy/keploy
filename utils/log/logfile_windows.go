package log

// Windows has neither O_NOFOLLOW nor O_NONBLOCK. ReadLogFile's identity check
// (os.SameFile against the file this run created) still refuses whatever
// replaced the log at its name.
const (
	oNoFollow = 0
	oNonBlock = 0
)
