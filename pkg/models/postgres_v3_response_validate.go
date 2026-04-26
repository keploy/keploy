package models

import "errors"

// ErrPostgresV3ResponseInvalid is the sentinel returned by
// PostgresV3Response.Validate() when the response carries a
// schema-level mutual-exclusion violation. Callers that want to
// distinguish validation failures from unrelated load errors can
// errors.Is against this sentinel; the wrapped message identifies
// which axes collided.
var ErrPostgresV3ResponseInvalid = errors.New("invalid PostgresV3Response")

// Validate enforces the wire-protocol-level mutual-exclusion rules
// across PostgresV3Response's payload axes.
//
// A single Postgres backend response carries exactly ONE of:
//
//   - DataRow traffic (Rows + RowDescription + CommandComplete)
//   - CopyOut traffic (CopyOutResponse + CopyData… + CopyDone)
//   - CopyIn handshake (CopyInResponse — client streams data back)
//   - FunctionCallResponse (legacy fastpath FunctionCall)
//
// The struct exposes each axis as an independent field for
// serialisation convenience, but the ONLY valid populated-field
// combinations on a single response are exactly one axis (plus
// any number of orthogonal Notices / Notifications / an Error).
//
// Without this enforcement, a malformed mock with two axes set
// would (a) serialise cleanly, (b) deserialise cleanly, (c) be
// loaded by the replay path, and (d) cause the wire emitter to
// nondeterministically pick one axis and silently drop the other —
// a class of "missing rows" / "missing CopyOut" replay bug that's
// hard to diagnose because no error surfaces. Validate() is the
// load-time guard against that.
//
// Note that Error is intentionally NOT considered a payload axis:
// a backend ErrorResponse can legitimately ride alongside a partial
// Rows burst (the server may have streamed some rows before raising
// the error). Notices and Notifications are similarly orthogonal —
// they interleave with any payload axis on the wire.
func (r *PostgresV3Response) Validate() error {
	if r == nil {
		return nil
	}

	hasRows := len(r.Rows) > 0
	hasCopyOut := r.CopyOut != nil
	hasCopyIn := r.CopyIn != nil
	hasFnCall := r.FunctionCall != nil

	// Pairwise mutual-exclusion checks. We report each violating
	// pair separately so a multiply-malformed response surfaces
	// every collision in one error rather than playing whack-a-mole
	// across reload cycles.
	var collisions []string
	if hasRows && hasCopyOut {
		collisions = append(collisions, "Rows and CopyOut populated together")
	}
	if hasRows && hasCopyIn {
		collisions = append(collisions, "Rows and CopyIn populated together")
	}
	if hasCopyOut && hasCopyIn {
		collisions = append(collisions, "CopyOut and CopyIn populated together")
	}
	if hasFnCall && hasRows {
		collisions = append(collisions, "FunctionCall and Rows populated together")
	}
	if hasFnCall && hasCopyOut {
		collisions = append(collisions, "FunctionCall and CopyOut populated together")
	}
	if hasFnCall && hasCopyIn {
		collisions = append(collisions, "FunctionCall and CopyIn populated together")
	}

	if len(collisions) == 0 {
		return nil
	}

	msg := "PostgresV3Response payload axes are mutually exclusive but found: "
	for i, c := range collisions {
		if i > 0 {
			msg += "; "
		}
		msg += c
	}
	return errors.Join(ErrPostgresV3ResponseInvalid, errors.New(msg))
}
