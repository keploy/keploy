//go:build !windows && !linux

package utils

// fionread asks a pipe how many bytes it holds: FIONREAD, _IOR('f', 127, int)
// in the BSDs' and darwin's <sys/filio.h>, which x/sys/unix does not name.
const fionread = 0x4004667f
