// Session-cookie middleware for the web dashboard (FOLLOWUPS #2
// phase 2). Gates every request to /api/v1/* under cluster mode so a
// browser without a valid cookie gets redirected to /login and a
// script caller without a bearer gets 401.
//
// Authenticated paths:
//   - Cookie (`sw_session`): web pod calls controller
//     /api/v1/auth/session, trusts the returned principal, stamps
//     opts.Token on the upstream proxy request.
//   - Bearer: web pod forwards the Authorization header verbatim so
//     agents and scripts bypass the cookie flow.
//
// Unauthenticated paths (static assets, the login page, /api/health
// on the web pod's own surface) go through without a middleware
// visit. See server.go for the route registration.
package web

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sessionAuthMiddleware runs on every /api/v1/* request and every SPA
// route. When the controller is unconfigured (laptop-local), it's a
// pure pass-through. In cluster mode, it:
//
//  1. Skips authentication if the request carries `Authorization: Bearer X`
//     (scripts + agents). The upstream controller enforces the bearer.
//  2. Else looks up the sw_session cookie against the controller's
//     /api/v1/auth/session endpoint.
//  3. If the cookie resolves to a valid principal, stamps it on the
//     request context and lets the proxy fire with opts.Token.
//  4. If the cookie is missing / invalid, redirects browsers to
//     /login?next=<path> and returns 401 for anything that looks
//     like an API/XHR call (Accept: application/json).
func sessionAuthMiddleware(opts HandlerOptions, next http.Handler) http.Handler {
	// Pass-through when the operator hasn't explicitly opted into the
	// login flow. Keeping this disabled-by-default preserves the
	// laptop-local dev loop: dev.sh starts with --controller set for
	// proxy reads, but no tokens minted, so a login redirect would
	// loop forever. Prod manifests opt in via --require-login.
	if !opts.RequireLogin || opts.ControllerURL == "" {
		return next
	}
	cache := newSessionCache(60 * time.Second)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bearer path takes precedence.
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			redirectOrUnauth(w, r)
			return
		}
		sess, err := cache.lookup(r.Context(), opts.ControllerURL, cookie.Value)
		if err != nil {
			clearSessionCookies(w)
			redirectOrUnauth(w, r)
			return
		}
		r = r.WithContext(contextWithWebPrincipal(r.Context(), sess))
		next.ServeHTTP(w, r)
	})
}

// redirectOrUnauth picks the right response based on what the caller
// probably is: a browser navigating the SPA gets a 302 to /login; an
// XHR or API call gets a 401 body.
func redirectOrUnauth(w http.ResponseWriter, r *http.Request) {
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") ||
		strings.HasPrefix(r.URL.Path, "/api/") {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	next := r.URL.Path
	if r.URL.RawQuery != "" {
		next = next + "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, "/login?next="+next, http.StatusSeeOther)
}

// webPrincipal is what sessionAuthMiddleware stamps on the request
// context. Handlers downstream can inspect it for audit or
// per-principal rendering.
type webPrincipal struct {
	Name      string
	Scopes    []string
	ExpiresAt time.Time
}

type webPrincipalCtxKey struct{}

func contextWithWebPrincipal(ctx context.Context, sess *sessionResp) context.Context {
	return context.WithValue(ctx, webPrincipalCtxKey{}, &webPrincipal{
		Name:      sess.Principal,
		Scopes:    sess.Scopes,
		ExpiresAt: time.Unix(sess.ExpiresAt, 0).UTC(),
	})
}

// WebPrincipalFromContext returns the logged-in user from the
// request context, if any.
func WebPrincipalFromContext(ctx context.Context) (*webPrincipal, bool) {
	p, ok := ctx.Value(webPrincipalCtxKey{}).(*webPrincipal)
	return p, ok
}

// --- session cache ---

type sessionCacheEntry struct {
	sess    *sessionResp
	expires time.Time
}

type sessionCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]*sessionCacheEntry
}

func newSessionCache(ttl time.Duration) *sessionCache {
	return &sessionCache{ttl: ttl, m: map[string]*sessionCacheEntry{}}
}

func (c *sessionCache) lookup(ctx context.Context, controllerURL, sessionID string) (*sessionResp, error) {
	c.mu.Lock()
	e := c.m[sessionID]
	c.mu.Unlock()
	if e != nil && time.Now().Before(e.expires) {
		return e.sess, nil
	}
	sess, err := controllerResolveSession(ctx, controllerURL, sessionID)
	if err != nil {
		c.mu.Lock()
		delete(c.m, sessionID)
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Lock()
	c.m[sessionID] = &sessionCacheEntry{
		sess:    sess,
		expires: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()
	return sess, nil
}
