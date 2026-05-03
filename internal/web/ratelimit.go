// Login + bootstrap rate-limit shim. In-process token-bucket keyed
// by source IP. Closes the obvious "credential-stuffing on /login"
// and "rapid first-admin-snipe on /login/bootstrap" abuse windows
// without pulling in Redis.
//
// Scope: per-IP buckets only. A determined attacker on a botnet
// will spread across IPs and slip past; that's a Cloudflare /
// nginx-rate-limit problem, not an in-process problem. We're
// closing the easy window where one IP brute-forces a single user.

package web

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginRateLimit is the per-IP cap for POST /login + POST
// /login/bootstrap. 10 requests per 60s is generous for humans,
// painful for credential-stuffing scripts, and harmless for the
// dashboard SPA (which only POSTs once per actual login).
const (
	loginRateBurst  = 10
	loginRateWindow = 60 * time.Second
)

type rateBucket struct {
	tokens     float64
	lastRefill time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rateBucket
	burst   float64
	window  time.Duration
}

func newRateLimiter(burst int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*rateBucket),
		burst:   float64(burst),
		window:  window,
	}
}

// allow reports whether a request from key should be served. Refills
// tokens proportionally to elapsed time. Returns false when the
// bucket is empty.
func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &rateBucket{tokens: l.burst, lastRefill: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.lastRefill)
	if elapsed > 0 {
		// Refill rate: burst tokens per window.
		b.tokens += float64(elapsed) / float64(l.window) * l.burst
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.lastRefill = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// gc drops bucket entries that have refilled to full and haven't
// been touched in window*2. Bounded growth without a per-request
// scan. Caller invokes from a periodic ticker; a missing call is
// safe but lets the map grow.
func (l *rateLimiter) gc(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if now.Sub(b.lastRefill) > 2*l.window && b.tokens >= l.burst {
			delete(l.buckets, k)
		}
	}
}

// rateLimitMiddleware wraps a handler with per-IP token-bucket
// limiting. 429 with a short Retry-After hint when the bucket runs
// dry. The middleware no-ops on GET requests since brute-force only
// makes sense against the form POST.
func rateLimitMiddleware(l *rateLimiter, next http.Handler) http.Handler {
	go func() {
		// Cheap reaper. The login surface is low-volume so a 5min
		// tick is plenty. Detached so the middleware is a one-call
		// install; cost is one goroutine per wrapped handler.
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for range t.C {
			l.gc(time.Now())
		}
	}()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		if !l.allow(clientIP(r), time.Now()) {
			w.Header().Set("Retry-After", "60")
			http.Error(w,
				"too many login attempts; try again in a minute",
				http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP picks the best-effort source IP for rate-limit keying.
// Honors X-Forwarded-For when set (we run behind nginx in prod) but
// trims the proxy chain to the first hop. Falls back to RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if ip := strings.TrimSpace(xff); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
