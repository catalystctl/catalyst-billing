package httpapi

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Per-principal rate limiting.
//
// The login throttle in internal/authn protects the *credential*. This protects the
// *surface*: a UI invites bulk actions, and a stuck retry loop in one browser tab must
// not be able to saturate the database for every other operator.
//
// It is a fixed window, in process. That is a deliberate trade: a fixed window is the
// simplest thing that bounds load, and it cannot be the thing that takes the panel down
// (no database round trip, no shared dependency). The cost is that the effective limit
// is per instance and can be exceeded by up to one window's worth at a boundary, which
// is acceptable for its purpose. It is not a quota or a billing meter.
//
// Limits are per route class rather than per route, so a new route inherits a sane
// budget by declaring what kind of route it is.

// Default per-minute budgets.
const (
	// limitRead bounds ordinary reads. An operator opening a list screen and paging
	// through it stays far below this; a poll loop that ignores the SSE endpoint hits it.
	limitRead = 600
	// limitMutation bounds state changes. Every mutation is audited and most take a row
	// lock, so the ceiling is much lower than for reads.
	limitMutation = 120
	// limitExport bounds expensive reads (audit and ledger dumps).
	limitExport = 30
)

// rateLimiter is a fixed-window counter keyed by (principal, class).
type rateLimiter struct {
	mu      sync.Mutex
	windows map[string]*rateWindow
	// now is injectable for tests.
	now func() time.Time
	// limits overrides the defaults; a zero entry falls back to the default.
	limits map[RateClass]int
}

type rateWindow struct {
	start time.Time
	count int
}

// newRateLimiter constructs a limiter.
func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		windows: make(map[string]*rateWindow),
		now:     func() time.Time { return time.Now().UTC() },
		limits:  make(map[RateClass]int),
	}
}

// limitFor returns the budget for a class.
func (l *rateLimiter) limitFor(class RateClass) int {
	if v, ok := l.limits[class]; ok && v > 0 {
		return v
	}
	switch class {
	case RateMutation:
		return limitMutation
	case RateExport:
		return limitExport
	case RateRead:
		return limitRead
	default:
		return 0
	}
}

// allow consumes one unit for (principal, class).
//
// It returns whether the request is allowed, how many remain, and when the window
// resets. A limit of zero means "no limit" and always allows.
func (l *rateLimiter) allow(principal string, class RateClass) (ok bool, remaining int, reset time.Time) {
	limit := l.limitFor(class)
	if limit <= 0 || principal == "" {
		return true, limit, time.Time{}
	}
	now := l.now()
	windowStart := now.Truncate(time.Minute).Add(time.Minute)

	l.mu.Lock()
	defer l.mu.Unlock()

	key := principal + "\x00" + string(class)
	w, ok2 := l.windows[key]
	if !ok2 || !now.Before(w.start) {
		w = &rateWindow{start: windowStart}
		l.windows[key] = w
	}
	if w.count >= limit {
		return false, 0, w.start
	}
	w.count++
	return true, limit - w.count, w.start
}

// sweep drops expired windows so the map does not grow with every principal ever seen.
func (l *rateLimiter) sweep() {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, w := range l.windows {
		if !now.Before(w.start) {
			delete(l.windows, k)
		}
	}
}

// withRateLimit applies the class budget to a route.
func (s *Server) withRateLimit(rt Route, next http.HandlerFunc) http.HandlerFunc {
	if s.rateLimiter == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		principal := principalID(r)
		if principal == "" {
			// Unauthenticated routes carry no class; nothing to meter.
			next(w, r)
			return
		}
		allowed, remaining, reset := s.rateLimiter.allow(principal, rt.Rate)
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(s.rateLimiter.limitFor(rt.Rate)))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		if !allowed {
			seconds := int(time.Until(reset).Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeProblem(w, http.StatusTooManyRequests, "rate_limited",
				"too many requests; retry after the window resets")
			return
		}
		next(w, r)
	}
}
