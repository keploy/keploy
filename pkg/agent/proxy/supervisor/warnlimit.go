package supervisor

import (
	"sync"
	"sync/atomic"
	"time"
)

// WarnLimiters lets one warning of each kind through per interval,
// process-wide, and counts the ones it holds back so the next one let through
// says how many it stands for (log it as sameWarningsHeldBack). It is for a
// warning that says something was not recorded: none of those may be silent,
// and a node recording hundreds of connections through one fault must not log
// hundreds of lines either. The ones held back are for the caller to log at
// Debug.
//
// Each kind (key) has its own interval and count, so a warning is never held
// back behind another kind's, and what it stands for is counted under its own
// kind. A key is never forgotten. Allow is for keys that are few and fixed (a
// message, a class of cause). AllowOr is for keys taken from text a caller
// does not control, such as the cause a reason names: it keeps at most
// maxOpenKinds of those apart, so they cannot grow without bound.
//
// The zero value is not usable; NewWarnLimiters makes one.
type WarnLimiters struct {
	every time.Duration
	now   func() time.Time // time.Now; tests set it
	kinds sync.Map         // key -> *warnLimiter

	// open counts the kinds AllowOr has added, up to maxOpenKinds.
	open atomic.Int64
}

// maxOpenKinds is how many kinds AllowOr keeps apart in one WarnLimiters.
// Past it, a key it has not seen is limited under its overflow kind.
const maxOpenKinds = 32

// NewWarnLimiters returns limiters that let one warning of each kind through
// every interval.
func NewWarnLimiters(every time.Duration) *WarnLimiters {
	return &WarnLimiters{every: every, now: time.Now}
}

// Allow reports whether a warning of kind key may be logged now, and, if it
// may, how many of that kind were held back since the last one that was. Keep
// key one of a few fixed kinds: each is kept for the life of the limiters.
func (ls *WarnLimiters) Allow(key string) (ok bool, heldBack uint64) {
	l, found := ls.kinds.Load(key)
	if !found {
		l, _ = ls.kinds.LoadOrStore(key, &warnLimiter{})
	}
	return l.(*warnLimiter).allow(ls.now(), ls.every)
}

// AllowOr is Allow for a key that need not be one of a few fixed kinds. The
// first maxOpenKinds keys it is handed each get their own kind. After that, a
// key it has not seen is limited as overflow, which must be one of a few fixed
// kinds, so its warning still goes through once per interval and is counted.
// It returns the kind the warning was limited as.
func (ls *WarnLimiters) AllowOr(key, overflow string) (ok bool, heldBack uint64, kind string) {
	if _, found := ls.kinds.Load(key); !found {
		switch {
		case ls.open.Add(1) > maxOpenKinds:
			ls.open.Add(-1)
			key = overflow
		default:
			if _, loaded := ls.kinds.LoadOrStore(key, &warnLimiter{}); loaded {
				// Another caller added it first, and counted it.
				ls.open.Add(-1)
			}
		}
	}
	ok, heldBack = ls.Allow(key)
	return ok, heldBack, key
}

// Reset forgets every warning let through and held back so far (tests). The
// kinds stay.
func (ls *WarnLimiters) Reset() {
	ls.kinds.Range(func(_, l any) bool {
		l.(*warnLimiter).reset()
		return true
	})
}

// warnLimiter is one kind's state in WarnLimiters.
type warnLimiter struct {
	mu   sync.Mutex
	last time.Time
	held uint64
}

func (l *warnLimiter) allow(now time.Time, every time.Duration) (bool, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && now.Sub(l.last) < every {
		l.held++
		return false, 0
	}
	held := l.held
	l.last, l.held = now, 0
	return true, held
}

func (l *warnLimiter) reset() {
	l.mu.Lock()
	l.last, l.held = time.Time{}, 0
	l.mu.Unlock()
}
