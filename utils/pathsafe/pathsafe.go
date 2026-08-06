// Package pathsafe holds the small path-safety predicates that are shared
// by every place a caller-supplied identifier (today: the test-set ID) is
// interpolated into a filesystem path.
//
// It deliberately depends on nothing but the standard library so the
// lowest-level packages (utils/log, the yaml stores) can all use the SAME
// definition of "safe name". Two hand-maintained copies of a security
// predicate drift — they already had, on the treatment of the empty string —
// and the next site to need one copies whichever it finds first.
package pathsafe

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidateSingleSegment reports whether name is a single, self-contained path
// element that is safe to filepath.Join onto a trusted base directory.
//
// It rejects:
//   - separators, checking BOTH '/' and '\\' regardless of GOOS, so a linux
//     build refuses exactly what a windows runtime would honour;
//   - ':' and any volume qualifier: filepath.IsAbs("C:") is false and Clean
//     leaves it alone, yet filepath.Join absorbs the drive qualifier on
//     windows and drops the base. filepath.VolumeName is windows-specific at
//     runtime (it returns "" for "C:" on linux), so the explicit ':' check is
//     what makes this cross-platform. It also catches UNC ("\\\\server") and
//     extended-length ("\\\\?\\C:") prefixes;
//   - "." and ".." verbatim, and anything not stable under filepath.Clean,
//     which is what rejects a "."/".." path ELEMENT while still allowing
//     legitimate names that merely contain the substring (e.g. "v1..v2").
//
// allowEmpty selects whether "" is a legal name: the debug-log sink uses ""
// to mean "rotate back to the origin file", while a mock store has no such
// meaning for it and must reject it.
//
// Callers wrap the returned error with their own context; the message names
// the offending value so the rejection is never silent.
func ValidateSingleSegment(name string, allowEmpty bool) error {
	if name == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("name must not be empty")
	}
	if name == "." || name == ".." ||
		strings.ContainsAny(name, `/\:`) ||
		filepath.VolumeName(name) != "" ||
		filepath.IsAbs(name) ||
		filepath.Clean(name) != name {
		return fmt.Errorf("%q must be a single-segment name (no separators, no drive/volume prefix, not %q or %q)", name, ".", "..")
	}
	return nil
}
