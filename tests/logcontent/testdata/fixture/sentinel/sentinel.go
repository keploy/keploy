// Package sentinel declares sentinels for testdata/fixture to log from
// another package: the scan reads each with its own package's types.
package sentinel

import "errors"

// ErrConstant is a sentinel of a constant text.
var ErrConstant = errors.New("a sentinel of another package")
