package models

// RepeatReadMark is the repeated read a test's own recording is one of
// (TestModeInfo.RepeatRead, MarkRepeatReads): the read's name, which keploy
// says when it answers the read once more, and which of the test's reads it
// is (At: 1 for the first read the replay names for the test, 2 for the
// second, and so on). Two reads are told apart by At, never by their names,
// which two reads may share: a namer names a read by its call's method, path
// and count. The zero mark, At 0, marks no read.
type RepeatReadMark struct {
	Name string
	At   int
}

// Marked reports whether m marks a read.
func (m RepeatReadMark) Marked() bool {
	return m.At > 0
}

// MarkRepeatReads is owned with each recording a repeated read of its test
// names (reads) replaced by a copy marked with that read
// (TestModeInfo.RepeatRead), which exact serving honours when the
// recordings prove it; owned's recordings are not changed. The k-th read is
// marked At k+1, so two reads named alike stay two. A recording two reads
// name carries the later one's mark.
func MarkRepeatReads(owned []*Mock, reads []RepeatRead) []*Mock {
	if len(reads) == 0 {
		return owned
	}
	read := map[string]RepeatReadMark{}
	for k, r := range reads {
		for _, n := range r.Mocks {
			read[n] = RepeatReadMark{Name: r.Name, At: k + 1}
		}
	}
	out := make([]*Mock, len(owned))
	for i, mk := range owned {
		out[i] = mk
		if mk == nil {
			continue
		}
		if r, ok := read[mk.Name]; ok {
			c := mk.ShallowCopy()
			c.TestModeInfo.RepeatRead = r
			out[i] = c
		}
	}
	return out
}
