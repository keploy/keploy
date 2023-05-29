package keploy

import "errors"

// KError stores the error for encoding and decoding as errorString has no exported fields due
// to gob wasn't able to encode the unexported fields.
type KError struct {
	Err error
}

// Error method returns error string stored in Err field of KError.
func (e *KError) Error() string {
	return e.Err.Error()
}

const version = 1

// GobEncode encodes the Err and returns the binary data.
func (e *KError) GobEncode() ([]byte, error) {
	r := make([]byte, 0)
	r = append(r, version)

	if e.Err != nil {
		r = append(r, e.Err.Error()...)
	}
	return r, nil
}

// GobDecode decodes the b([]byte) into error struct.
func (e *KError) GobDecode(b []byte) error {
	if b[0] != version {
		return errors.New("gob decode of errors.errorString failed: unsupported version")
	}
	if len(b) == 1 {
		e.Err = nil
	} else {
		str := string(b[1:])
		e.Err = errors.New(str)
	}

	return nil
}
