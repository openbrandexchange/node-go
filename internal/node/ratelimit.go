package node

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/openbrandexchange/node-go/internal/problem"
)

// rateLimiter is a fixed-window counter per key (single process; a node
// runs as one instance).
type rateLimiter struct {
	mu      sync.Mutex
	windows map[string]*window
	last    time.Time
}

type window struct {
	start time.Time
	count int
}

func newRateLimiter() *rateLimiter { return &rateLimiter{windows: map[string]*window{}} }

// allow counts one hit for key; it returns remaining hits and seconds until
// the window resets.
func (l *rateLimiter) allow(key string, limit int, per time.Duration, now time.Time) (ok bool, remaining int, reset int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.last) > time.Minute {
		for k, w := range l.windows {
			if now.Sub(w.start) > 2*time.Hour {
				delete(l.windows, k)
			}
		}
		l.last = now
	}
	w := l.windows[key]
	if w == nil || now.Sub(w.start) >= per {
		w = &window{start: now}
		l.windows[key] = w
	}
	resetIn := int((per - now.Sub(w.start)).Seconds())
	if resetIn < 1 {
		resetIn = 1
	}
	if w.count >= limit {
		return false, 0, resetIn
	}
	w.count++
	return true, limit - w.count, resetIn
}

// limit applies a rate limit and writes RateLimit-* headers. It returns
// false (and writes 429) when exceeded.
func (a *App) limit(w http.ResponseWriter, key string, limit int, per time.Duration) bool {
	if a.Cfg.TestRateLimit > 0 && per == time.Minute && limit > a.Cfg.TestRateLimit {
		limit = a.Cfg.TestRateLimit
	}
	ok, remaining, reset := a.limiter.allow(key, limit, per, time.Now())
	h := w.Header()
	h.Set("RateLimit-Limit", strconv.Itoa(limit))
	h.Set("RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("RateLimit-Reset", strconv.Itoa(reset))
	if !ok {
		p := problem.New("rate_limited", "Too many requests; try again in %d seconds.", reset)
		p.RetryAfter = &reset
		problem.Write(w, p)
	}
	return ok
}
