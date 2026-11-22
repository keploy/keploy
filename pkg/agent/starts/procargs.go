package starts

import "bytes"

// procArgs is the command line's words (argv) that b, a macOS process's
// kern.procargs2, holds (sysProc.Command on darwin). b is argc, a 4-byte
// int in the Mac's byte order (little-endian: arm64, amd64), then the
// strings the kernel copied out at exec (xnu's exec_extract_strings and
// exec_copyout_strings): the executable's path and its NUL, NULs that pad
// it to the next 8 bytes from where it begins (a 64-bit process's pointer's
// size), then argc words, each ending in a NUL, then the environment's.
//
// The padding is read by its size, not as every NUL after the path: an
// empty argv[0] (execve with argv[0] "", exec -a with an empty name) is a
// NUL there too.
// Skipping it with the padding would read argv[1] as the program and the
// environment's first word as the last argument. A b whose bytes up to the
// next 8 after the path are not all NUL is laid out otherwise (a 32-bit
// process's, padded to 4): its words are read from the first byte past the
// NULs after its path, as ps reads them.
func procArgs(b []byte) []string {
	if len(b) <= 4 {
		return nil
	}
	argc := int(b[0]) | int(b[1])<<8 | int(b[2])<<16 | int(b[3])<<24
	rest := b[4:]
	path := bytes.IndexByte(rest, 0)
	if path < 0 {
		return nil
	}
	start := (path + 1 + 7) &^ 7
	if start > len(rest) || len(bytes.TrimLeft(rest[path+1:start], "\x00")) > 0 {
		start = path + 1
		for start < len(rest) && rest[start] == 0 {
			start++
		}
	}
	rest = rest[start:]
	var argv []string
	for len(argv) < argc && len(rest) > 0 {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			argv = append(argv, string(rest))
			break
		}
		argv = append(argv, string(rest[:i]))
		rest = rest[i+1:]
	}
	return argv
}
