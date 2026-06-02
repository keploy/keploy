// Package supervisor wraps a parser goroutine with the three safety features
// that keep a buggy parser from affecting user traffic: a panic firewall that
// recovers panics without closing real sockets, an activity watchdog that
// declares a parser hung when it stops making progress while there is
// pending work, and goroutine accounting so parser-spawned helpers are
// cancelled when the parser aborts.
//
// The supervisor is per-connection (one instance per active session) and
// deliberately does not own real sockets or the forwarding path — those
// belong to the relay. Callers inspect the returned Result to decide
// whether to fall through to raw passthrough. See PLAN.md (section 3.6)
// at the repo root for the architectural context.
package supervisor
