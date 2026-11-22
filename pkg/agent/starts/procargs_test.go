package starts

import (
	"bytes"
	"reflect"
	"testing"
)

// procargs2 is a kern.procargs2 buffer as xnu lays one out for a 64-bit
// process: argc (little-endian), the executable's path and its NUL, NULs to
// the next 8 bytes from the path's start, then argv's words and env's, each
// ending in a NUL.
func procargs2(path string, argv, env []string) []byte {
	b := []byte{byte(len(argv)), byte(len(argv) >> 8), 0, 0}
	b = append(b, path...)
	b = append(b, 0)
	for (len(b)-4)%8 != 0 {
		b = append(b, 0)
	}
	for _, w := range append(append([]string{}, argv...), env...) {
		b = append(b, w...)
		b = append(b, 0)
	}
	return b
}

// macOS's command line is read word by word, its path's padding by its size:
// an empty argv[0] stays argv[0], so argv[1] is not taken for the program,
// nor the environment's first word for the last argument; a path that
// needs no padding, or seven bytes of it, reads alike. A buffer whose
// padding holds a word (a 32-bit process's, padded to 4) is read from the
// first byte past the NULs after its path.
func TestAMacProcessesWordsAreReadWithAnEmptyArgv0(t *testing.T) {
	env := []string{"HOME=/Users/dev", "TOKEN=s3cret"}
	for _, tc := range []struct {
		name string
		b    []byte
		want []string
	}{
		{"a path with padding", procargs2("/usr/bin/python3", []string{"python3", "client.py"}, env), []string{"python3", "client.py"}},
		{"a path with no padding", procargs2("/bin/ls", []string{"ls", "-l"}, env), []string{"ls", "-l"}},
		{"an empty argv[0], a path with no padding", procargs2("/bin/ls", []string{"", "dep.keploy.test:18999"}, env), []string{"", "dep.keploy.test:18999"}},
		{"an empty argv[0], a path with padding", procargs2("/usr/bin/python3", []string{"", "dep.keploy.test:18999"}, env), []string{"", "dep.keploy.test:18999"}},
		{"two empty words first", procargs2("/opt/app/run", []string{"", "", "x"}, env), []string{"", "", "x"}},
		{"a 32-bit process's padding", append([]byte{2, 0, 0, 0}, []byte("/bin/abc\x00\x00\x00\x00prog\x00x\x00E=1\x00")...), []string{"prog", "x"}},
		{"fewer words than argc", append([]byte{3, 0, 0, 0}, []byte("/bin/ls\x00ls\x00-l")...), []string{"ls", "-l"}},
		{"no words", procargs2("/bin/ls", nil, nil), nil},
		{"no path's end", append([]byte{1, 0, 0, 0}, []byte("/bin/ls")...), nil},
		{"argc alone", []byte{1, 0, 0, 0}, nil},
	} {
		if got := procArgs(tc.b); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: procArgs(%q) = %q; want %q", tc.name, tc.b, got, tc.want)
		}
	}
	if b := procargs2("/bin/ls", []string{"", "x"}, env); !bytes.HasPrefix(b[4:], []byte("/bin/ls\x00\x00x\x00")) {
		t.Fatalf("the buffer's layout is %q; want /bin/ls, its NUL, then argv[0]'s NUL", b)
	}
}
