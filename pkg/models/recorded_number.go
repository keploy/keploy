package models

// RecordedNumber is the number mockdb gave a mock as it wrote it: it names
// each mock "mock-<N>" from one counter as it appends the mock to the set's
// file, so N, decimal digits, is the mock's place in the recording. ok is
// false for a name of any other form (written by hand, or by another tool),
// and for an N of more than 19 digits, which no counter reaches.
func RecordedNumber(name string) (n uint64, ok bool) {
	const prefix = "mock-"
	if len(name) <= len(prefix) || name[:len(prefix)] != prefix || len(name)-len(prefix) > 19 {
		return 0, false
	}
	for i := len(prefix); i < len(name); i++ {
		c := name[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	return n, true
}

// CompareRecordedNumbers orders two mocks of one set, by name, in the order
// mockdb wrote them (RecordedNumber): mock-2 before mock-10, which a compare
// of the names as text puts the other way round. A numbered name sorts
// before one with no number, which has no place in the recording; it is 0
// when neither name is numbered or both carry one number, and the caller
// breaks the tie. Several mocks of one request time are what a client
// writing many requests at once (an HTTP/1.1 pipeline) records, so the
// order of a tie is the order their answers are served in.
func CompareRecordedNumbers(a, b string) int {
	na, oka := RecordedNumber(a)
	nb, okb := RecordedNumber(b)
	switch {
	case oka != okb:
		if oka {
			return -1
		}
		return 1
	case na < nb:
		return -1
	case na > nb:
		return 1
	}
	return 0
}
