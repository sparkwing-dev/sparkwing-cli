// Package web serves the sparkwing dashboard. It exposes:
//
//   - JSON API over the orchestrator's state store at /api/runs/*
//     (the read surface the Next.js dashboard consumes)
//   - /api/v1/triggers proxy to the controller (cluster mode)
//   - the embedded Next.js bundle at /
//
// The bundle lives under pkg/orchestrator/web/next-out/ and is
// populated by `wing install` (or the Dockerfile) before `go build`
// runs.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sparkwing-dev/sparkwing-sdk/orchestrator"
	"github.com/sparkwing-dev/sparkwing-sdk/orchestrator/store"
	"github.com/sparkwing-dev/sparkwing-cli/pkg/pipelines"
)

// pipelineMeta caches per-pipeline metadata (tags, triggers) derived
// from the nearest .sparkwing/pipelines.yaml. Best-effort: an empty
// map is returned when the yaml is missing or invalid.
type pipelineMeta struct {
	tags map[string][]string
}

func loadPipelineMeta() *pipelineMeta {
	meta := &pipelineMeta{tags: map[string][]string{}}
	cwd, err := os.Getwd()
	if err != nil {
		return meta
	}
	_, cfg, err := pipelines.Discover(cwd)
	if err != nil {
		return meta
	}
	for _, p := range cfg.Pipelines {
		meta.tags[p.Name] = p.Tags
	}
	return meta
}

//go:embed all:next-out
var nextBundle embed.FS

// HandlerOptions bundles everything the dashboard handler needs.
// Zero value is the local-mode default.
type HandlerOptions struct {
	Reader        Reader
	Paths         orchestrator.Paths
	LogSource     LogSource
	ControllerURL string // if set, /api/v1/* proxies to this URL
	LogsURL       string // sparkwing-logs base URL (for /api/v1/health/services probe)
	Token         string // controller bearer token (cluster mode)
	// APIURL injected into the SPA HTML as window.__SPARKWING_API_URL__.
	// Empty -> same-origin (what `sparkwing web` wants).
	APIURL string
	// ExtraServices augments the default controller/logs health probes
	// with any additional endpoints the operator wants on the
	// dashboard (cache, dind, pool, etc.).
	ExtraServices []HealthService
	// RequireLogin gates the browser-facing surface behind the
	// session-cookie flow when true. Pass false (the default) for
	// laptop-local dev, where an empty tokens table means there are
	// no credentials to log in with and the login redirect would
	// produce a dead-end loop.
	RequireLogin bool
}

// Serve starts the dashboard bound to addr, reading state from the
// local SQLite store at paths.StateDB(). Backwards-compatible
// behavior for local mode.
func Serve(ctx context.Context, paths orchestrator.Paths, addr string) error {
	if err := paths.EnsureRoot(); err != nil {
		return err
	}
	st, err := store.Open(paths.StateDB())
	if err != nil {
		return err
	}
	defer st.Close()
	return ServeWith(ctx, storeReader{st: st}, paths, addr)
}

// ServeWith starts the dashboard against any Reader. Use this to
// point at a remote controller (Reader = *client.Client) while
// keeping paths as the local log-file root. Caller owns the
// Reader's lifecycle.
func ServeWith(ctx context.Context, reader Reader, paths orchestrator.Paths, addr string) error {
	if err := paths.EnsureRoot(); err != nil {
		return err
	}
	return ServeWithOptions(ctx, HandlerOptions{Reader: reader, Paths: paths}, addr)
}

// ServeWithOptions is the cluster-mode entry point: bundles reader,
// log source, controller URL + token so the SPA can POST /api/v1/*
// through the dashboard pod.
func ServeWithOptions(ctx context.Context, opts HandlerOptions, addr string) error {
	if err := opts.Paths.EnsureRoot(); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:         addr,
		Handler:      HandlerFromOptions(opts),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(os.Stderr, "sparkwing web: serving http://%s\n", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler is the legacy entry point; prefer HandlerFromOptions for
// new callers that need controller proxying.
func Handler(reader Reader, paths orchestrator.Paths, logSource LogSource) http.Handler {
	return HandlerFromOptions(HandlerOptions{
		Reader:    reader,
		Paths:     paths,
		LogSource: logSource,
	})
}

// HandlerFromOptions returns the full dashboard HTTP handler.
func HandlerFromOptions(opts HandlerOptions) http.Handler {
	meta := loadPipelineMeta()
	logSource := opts.LogSource
	if logSource == nil {
		logSource = diskLogSource{paths: opts.Paths}
	}

	// Build the authenticated inner mux first, then wrap it with the
	// session-cookie middleware. Login + logout are registered on the
	// outer router so they bypass the middleware (catch-22 otherwise).
	authedMux := http.NewServeMux()
	authedMux.HandleFunc("/api/runs", listRunsHandler(opts.Reader, meta))
	authedMux.HandleFunc("/api/runs/", runDetailHandler(opts.Reader, logSource, meta))

	// Session B: aggregate health probe. Handled at the dashboard
	// layer (not the controller) because only the dashboard knows
	// the URLs of every sibling service in a deployment.
	services := append(defaultServices(opts, opts.LogsURL), opts.ExtraServices...)
	authedMux.HandleFunc("/api/v1/health/services", healthServicesHandler(services, opts.Token))

	// Session C: pipeline registry.
	authedMux.HandleFunc("/api/v1/pipelines", pipelinesHandler())

	// Proxy /api/v1/logs/* to the logs-service; everything else on
	// /api/v1/* goes to the controller. Routes registered in
	// decreasing specificity so Go's ServeMux picks the right one.
	if opts.LogsURL != "" {
		authedMux.Handle("/api/v1/logs/", controllerProxy(opts.LogsURL, opts.Token))
	}
	if opts.ControllerURL != "" {
		authedMux.Handle("/api/v1/", controllerProxy(opts.ControllerURL, opts.Token))
	} else {
		authedMux.HandleFunc("/api/v1/", notImplementedHandler)
	}

	subFS, err := fs.Sub(nextBundle, "next-out")
	if err != nil {
		panic(fmt.Sprintf("web: embed fs.Sub failed: %v", err))
	}
	authedMux.Handle("/", spaHandler(subFS, opts))

	// Outer router: login/logout + the always-open /api/health +
	// everything else behind session auth.
	router := http.NewServeMux()
	router.HandleFunc("/api/health", healthHandler)
	router.HandleFunc("GET /login", loginPageHandler(opts))
	// Rate-limit POST /login + /login/bootstrap with one shared bucket
	// per source IP. A determined attacker probing both endpoints
	// shouldn't get to spend its budget twice.
	loginLimiter := newRateLimiter(loginRateBurst, loginRateWindow)
	router.Handle("POST /login",
		rateLimitMiddleware(loginLimiter, http.HandlerFunc(loginSubmitHandler(opts))))
	// First-visit signup (RUN-013): a fresh cluster's /login renders a
	// "create first admin" form that posts here; we forward to the
	// controller's unauthenticated POST /api/v1/users bootstrap path.
	router.Handle("POST /login/bootstrap",
		rateLimitMiddleware(loginLimiter, http.HandlerFunc(bootstrapSubmitHandler(opts))))
	router.HandleFunc("POST /logout", logoutHandler(opts))
	router.Handle("/", sessionAuthMiddleware(opts, authedMux))
	return router
}

// spaHandler serves the Next.js static export with two tweaks:
//   - HTML files get templated so the Go server can inject the
//     runtime bearer token + API URL into window globals
//   - unknown paths (client-side routes like /#/runs/<id>) fall
//     through to index.html so the SPA handles them
//
// Route resolution matches what `next build --output export` actually
// emits in Next 16: each page is a top-level `<route>.html` file, and
// the same-named `<route>/` directory contains Turbopack metadata
// (`__next.*.txt`) rather than an index.html. Earlier versions emitted
// `<route>/index.html` so we keep that fallback.
func spaHandler(bundleFS fs.FS, opts HandlerOptions) http.Handler {
	fileServer := http.FileServer(http.FS(bundleFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		p = strings.TrimSuffix(p, "/")
		if p == "" {
			serveTemplatedHTML(w, r, bundleFS, "index.html", opts)
			return
		}

		if strings.HasSuffix(p, ".html") && isTemplatedPath(p) {
			serveTemplatedHTML(w, r, bundleFS, p, opts)
			return
		}

		// Preferred: Next 16 emits top-level <route>.html. Handle this
		// before the fs.Stat(p) directory check because the export
		// also creates a same-named directory with Turbopack internals
		// that http.FileServer would 301-redirect into a dead end.
		if _, err := fs.Stat(bundleFS, p+".html"); err == nil {
			serveTemplatedHTML(w, r, bundleFS, p+".html", opts)
			return
		}

		// Legacy layout: <route>/index.html (Next <= 15).
		if _, err := fs.Stat(bundleFS, p+"/index.html"); err == nil {
			serveTemplatedHTML(w, r, bundleFS, p+"/index.html", opts)
			return
		}

		// Static asset under _next/, favicons, fonts, etc.
		if info, err := fs.Stat(bundleFS, p); err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}

		// Fallthrough: SPA client-side route. Serve the root page.
		serveTemplatedHTML(w, r, bundleFS, "index.html", opts)
	})
}

// isTemplatedPath returns true for HTML files that contain the
// runtime-config markers. All top-level Next.js pages do; static
// assets (favicon, fonts, chunks) don't.
func isTemplatedPath(p string) bool {
	// All top-level .html files emitted by Next export carry the
	// markers because they share layout.tsx. Narrow here if that
	// changes.
	return !strings.HasPrefix(p, "_next/") && !strings.HasPrefix(p, "next-dev/")
}

func serveTemplatedHTML(w http.ResponseWriter, _ *http.Request, bundleFS fs.FS, name string, opts HandlerOptions) {
	raw, err := fs.ReadFile(bundleFS, name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	// JSON-encode values so quotes/backslashes in a token don't
	// break the <script> literal. The markers are JSON strings in
	// the template, so the substituted value must include its own
	// surrounding quotes -- we wrote them with quotes in layout.tsx,
	// so replace the inside.
	body := bytes.ReplaceAll(raw,
		[]byte("__SPARKWING_TOKEN_MARKER__"),
		[]byte(jsStringEscape(opts.Token)))
	body = bytes.ReplaceAll(body,
		[]byte("__SPARKWING_API_URL_MARKER__"),
		[]byte(jsStringEscape(opts.APIURL)))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// jsStringEscape escapes only the subset of characters that would
// break out of the double-quoted JS string literal in layout.tsx.
// Tokens in sparkwing are ASCII; still, be defensive.
func jsStringEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '<': // defensive: avoid breaking out of <script>
			b.WriteString(`<`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func controllerProxy(controllerURL, token string) http.Handler {
	u, err := url.Parse(controllerURL)
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, fmt.Sprintf("bad controller URL: %v", err), http.StatusInternalServerError)
		})
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		// Controller sits behind the proxy; it doesn't need the
		// Host header munged further.
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	return proxy
}

func notImplementedHandler(w http.ResponseWriter, _ *http.Request) {
	http.Error(w,
		"this endpoint requires --controller mode; start the dashboard with --controller URL",
		http.StatusNotImplemented)
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// listRunsHandler returns recent runs, newest first.
func listRunsHandler(reader Reader, meta *pipelineMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		filter := store.RunFilter{Limit: limit}
		if v := r.URL.Query().Get("pipeline"); v != "" {
			filter.Pipelines = strings.Split(v, ",")
		}
		if v := r.URL.Query().Get("status"); v != "" {
			filter.Statuses = strings.Split(v, ",")
		}
		if v := r.URL.Query().Get("since"); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				filter.Since = time.Now().Add(-d)
			}
		}
		runs, err := reader.ListRuns(r.Context(), filter)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"runs": toAPIRuns(runs, meta),
		})
	}
}

// runDetailHandler serves:
//
//	/api/runs/<id>           -> run detail with nodes
//	/api/runs/<id>/logs      -> all node logs for the run
//	/api/runs/<id>/logs/<n>  -> single node's log contents
//	/api/runs/<id>/logs/<n>/stream -> SSE of log lines
//	/api/runs/<id>/cancel    -> POST to request cancellation
//
// State comes from `reader`; log contents come from `logSource`
// (disk in local mode, HTTP to sparkwing-logs in cluster mode).
func runDetailHandler(reader Reader, logSource LogSource, meta *pipelineMeta) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/runs/")
		parts := strings.Split(rest, "/")
		if parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		runID := parts[0]
		switch {
		case len(parts) == 1:
			serveRunDetail(reader, meta, w, r, runID)
		case len(parts) == 2 && parts[1] == "logs":
			serveLogs(reader, logSource, w, r, runID, "")
		case len(parts) == 3 && parts[1] == "logs":
			serveLogs(reader, logSource, w, r, runID, parts[2])
		case len(parts) == 4 && parts[1] == "logs" && parts[3] == "stream":
			serveLogStream(logSource, w, r, runID, parts[2])
		case len(parts) == 3 && parts[1] == "events" && parts[2] == "stream":
			serveEventsStream(reader, w, r, runID)
		case len(parts) == 2 && parts[1] == "cancel":
			serveCancel(reader, w, r, runID)
		case len(parts) == 2 && parts[1] == "paused":
			serveListPauses(reader, w, r, runID)
		case len(parts) == 4 && parts[1] == "nodes" && parts[3] == "release":
			serveReleasePause(reader, w, r, runID, parts[2])
		default:
			http.NotFound(w, r)
		}
	}
}

// serveListPauses returns every debug_pauses row for the run as a
// JSON array. The dashboard filters to open pauses (released_at ==
// null) on the client; sending the full history preserves audit
// visibility without a second request.
func serveListPauses(reader Reader, w http.ResponseWriter, r *http.Request, runID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pauses, err := reader.ListDebugPauses(r.Context(), runID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if pauses == nil {
		pauses = []*store.DebugPause{}
	}
	writeJSON(w, http.StatusOK, pauses)
}

// serveReleasePause is the dashboard's Release button endpoint. Maps
// to sparkwing.ReleaseDebugPause with release_kind="manual" and the
// dashboard's principal (or "dashboard" when unauthenticated local
// dev) as released_by.
func serveReleasePause(reader Reader, w http.ResponseWriter, r *http.Request, runID, nodeID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	releasedBy := "dashboard"
	if v := r.Header.Get("X-Sparkwing-User"); v != "" {
		releasedBy = v
	}
	err := reader.ReleaseDebugPause(r.Context(), runID, nodeID, releasedBy, store.PauseReleaseManual)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveCancel forwards an operator cancel request to the underlying
// reader. Returns 501 for readers that don't support cancellation
// (local mode) so the dashboard can hide the button next time.
func serveCancel(reader Reader, w http.ResponseWriter, r *http.Request, runID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	err := reader.CancelRun(r.Context(), runID)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrCancelNotSupported):
		writeErr(w, http.StatusNotImplemented, err)
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
	default:
		writeErr(w, http.StatusBadGateway, err)
	}
}

func serveRunDetail(reader Reader, meta *pipelineMeta, w http.ResponseWriter, r *http.Request, runID string) {
	run, err := reader.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	nodes, err := reader.ListNodes(r.Context(), runID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"run":   toAPIRun(run, meta),
		"nodes": toAPINodes(nodes, metaFromSnapshot(run.PlanSnapshot)),
	})
}

// nodeMeta carries per-node author-intent fields that live on the
// plan snapshot rather than the store.Node row: cluster memberships
// (Groups, from sparkwing.Group(plan, name, ...)), the dynamic flag (explicit
// `.Dynamic()` or ExpandFrom source), whether the node is gated by
// an approval (plan.Approval(...)), the recovery-edge id, the active
// Plan-layer modifiers (Retry / Timeout / RunsOn / Cache), and the
// inner Work tree (steps + spawn declarations). The dashboard reads
// each of these to render the nested Plan -> Node -> Work -> Step
// view.
type nodeMeta struct {
	Groups      []string
	Dynamic     bool
	Approval    bool
	OnFailureOf string
	Modifiers   *snapshotNodeModifiers
	Work        *snapshotNodeWork
}

// snapshotNodeModifiers mirrors the orchestrator's snapshotModifiers
// wire shape locally so the web pkg doesn't import the orchestrator.
// Kept lossless so the dashboard sees every label the explain CLI
// shows.
type snapshotNodeModifiers struct {
	Retry           int      `json:"retry,omitempty"`
	RetryBackoffMS  int64    `json:"retry_backoff_ms,omitempty"`
	RetryAuto       bool     `json:"retry_auto,omitempty"`
	TimeoutMS       int64    `json:"timeout_ms,omitempty"`
	RunsOn          []string `json:"runs_on,omitempty"`
	CacheKey        string   `json:"cache_key,omitempty"`
	CacheMax        int      `json:"cache_max,omitempty"`
	CacheOnLimit    string   `json:"cache_on_limit,omitempty"`
	Inline          bool     `json:"inline,omitempty"`
	Optional        bool     `json:"optional,omitempty"`
	ContinueOnError bool     `json:"continue_on_error,omitempty"`
	OnFailure       string   `json:"on_failure,omitempty"`
	HasBeforeRun    bool     `json:"has_before_run,omitempty"`
	HasAfterRun     bool     `json:"has_after_run,omitempty"`
	HasSkipIf       bool     `json:"has_skip_if,omitempty"`
}

type snapshotNodeWork struct {
	Steps      []snapshotNodeStep      `json:"steps,omitempty"`
	Spawns     []snapshotNodeSpawn     `json:"spawns,omitempty"`
	SpawnEach  []snapshotNodeSpawnEach `json:"spawn_each,omitempty"`
	ResultStep string                  `json:"result_step,omitempty"`
}

type snapshotNodeStep struct {
	ID        string   `json:"id"`
	Needs     []string `json:"needs,omitempty"`
	IsResult  bool     `json:"is_result,omitempty"`
	HasSkipIf bool     `json:"has_skip_if,omitempty"`
}

type snapshotNodeSpawn struct {
	ID         string            `json:"id"`
	Needs      []string          `json:"needs,omitempty"`
	TargetJob  string            `json:"target_job,omitempty"`
	TargetWork *snapshotNodeWork `json:"target_work,omitempty"`
	HasSkipIf  bool              `json:"has_skip_if,omitempty"`
}

type snapshotNodeSpawnEach struct {
	ID               string            `json:"id"`
	Needs            []string          `json:"needs,omitempty"`
	TargetJob        string            `json:"target_job,omitempty"`
	ItemTemplateWork *snapshotNodeWork `json:"item_template_work,omitempty"`
	Note             string            `json:"note,omitempty"`
}

// metaFromSnapshot pulls per-node fields out of the stored plan
// snapshot JSON so the API can return them alongside each node. The
// snapshot is authored by marshalPlanSnapshot in pkg/orchestrator; we
// decode into a local shape rather than importing the orchestrator
// types. Returns nil for empty/unparseable snapshots — callers treat
// that the same as "no metadata" and the dashboard falls back to
// rendering nodes without adornments.
func metaFromSnapshot(snapshot []byte) map[string]nodeMeta {
	if len(snapshot) == 0 {
		return nil
	}
	var parsed struct {
		Nodes []struct {
			ID       string   `json:"id"`
			Groups   []string `json:"groups"`
			Dynamic  bool     `json:"dynamic"`
			Approval *struct {
				Message string `json:"message"`
			} `json:"approval"`
			OnFailureOf string                 `json:"on_failure_of"`
			Modifiers   *snapshotNodeModifiers `json:"modifiers"`
			Work        *snapshotNodeWork      `json:"work"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(snapshot, &parsed); err != nil {
		return nil
	}
	out := make(map[string]nodeMeta, len(parsed.Nodes))
	for _, n := range parsed.Nodes {
		hasApproval := n.Approval != nil
		if len(n.Groups) == 0 && !n.Dynamic && !hasApproval && n.OnFailureOf == "" && n.Modifiers == nil && n.Work == nil {
			continue
		}
		out[n.ID] = nodeMeta{
			Groups:      n.Groups,
			Dynamic:     n.Dynamic,
			Approval:    hasApproval,
			OnFailureOf: n.OnFailureOf,
			Modifiers:   n.Modifiers,
			Work:        n.Work,
		}
	}
	return out
}

// serveLogStream proxies the logs-service SSE stream through the
// dashboard. The body is passed through verbatim so the browser's
// EventSource handles framing. Closing either end tears the whole
// thing down via context cancellation.
func serveLogStream(logSource LogSource, w http.ResponseWriter, r *http.Request, runID, nodeID string) {
	body, err := logSource.StreamNode(r.Context(), runID, nodeID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if body == nil {
		// Source doesn't support streaming (e.g. disk in local mode).
		// 501 lets the dashboard fall back to polling.
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	defer body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("streaming not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	buf := make([]byte, 4096)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

// serveEventsStream tails the run's events table as an SSE stream.
// Delivers the backlog after Last-Event-ID (or 0), then polls every
// 250ms for new rows until the run reaches a terminal status, at
// which point it drains any trailing events and closes. Each record
// is emitted as:
//
//	id: <seq>
//	event: <kind>
//	data: {"run_id":"...","seq":N,"node_id":"...","kind":"...","ts":"...","payload":...}
//
// Browsers echo the last-seen id back as Last-Event-ID on reconnect,
// so resuming mid-stream is a simple "seq > afterSeq" read. Clients
// that can't open EventSource (401, 502, etc.) fall back to the
// existing getRun polling.
func serveEventsStream(reader Reader, w http.ResponseWriter, r *http.Request, runID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("streaming not supported"))
		return
	}

	// Verify the run exists up-front so a typo returns 404 instead of
	// an open stream that never produces anything.
	run, err := reader.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	afterSeq := parseLastEventID(r.Header.Get("Last-Event-ID"))

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disable nginx's proxy buffering so events land in the browser
	// within one poll tick instead of after a buffer fills.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Open-comment keeps the connection alive while we wait on the
	// first poll. EventSource tolerates leading comment lines.
	_, _ = w.Write([]byte(": open\n\n"))
	flusher.Flush()

	ctx := r.Context()
	const (
		pollInterval = 250 * time.Millisecond
		pageSize     = 500
		// Run-status recheck cadence: we poll events frequently but
		// only re-read the run row every N ticks so a long-running
		// stream doesn't hammer the store for a single column we
		// only need for termination.
		runStatusEveryN = 8
		heartbeatEvery  = 20 * time.Second
	)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	tick := 0
	lastHB := time.Now()
	terminal := isRunTerminal(run.Status)

	for {
		events, err := reader.ListEventsAfter(ctx, runID, afterSeq, pageSize)
		if err != nil {
			// Don't surface the error body in an already-open SSE stream;
			// a close is the cleanest signal to the client. The
			// EventSource onerror handler kicks in and triggers the
			// fallback path.
			return
		}
		for _, ev := range events {
			if !writeEventSSE(w, ev) {
				return
			}
			afterSeq = ev.Seq
		}
		if len(events) > 0 {
			flusher.Flush()
			lastHB = time.Now()
		}

		// If we know the run is done AND we just drained to empty, the
		// stream's job is finished. Send a final end-of-stream hint so
		// the client can close cleanly without waiting for onerror.
		if terminal && len(events) == 0 {
			_, _ = w.Write([]byte("event: stream_end\ndata: {}\n\n"))
			flusher.Flush()
			return
		}

		// Emit a keepalive comment if nothing went out recently — some
		// proxies (including dev-mode Next.js) reap idle SSE streams
		// after ~30s otherwise.
		if time.Since(lastHB) >= heartbeatEvery {
			if _, werr := w.Write([]byte(": keepalive\n\n")); werr != nil {
				return
			}
			flusher.Flush()
			lastHB = time.Now()
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		tick++
		if tick%runStatusEveryN == 0 && !terminal {
			if fresh, rerr := reader.GetRun(ctx, runID); rerr == nil && fresh != nil {
				terminal = isRunTerminal(fresh.Status)
			}
		}
	}
}

// parseLastEventID interprets the browser-sent Last-Event-ID header.
// Non-numeric or missing values resume from seq 0 (full backlog); a
// valid positive int resumes from just after that seq.
func parseLastEventID(h string) int64 {
	if h == "" {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(h), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// isRunTerminal reports whether a run status means "no more events
// will be emitted" — used to decide when the SSE handler can close.
// Mirrors the set the dashboard treats as non-running in its filter
// bar (success/failed/cancelled). Unknown statuses are treated as
// still-running so the stream stays open.
func isRunTerminal(status string) bool {
	switch status {
	case "success", "failed", "cancelled":
		return true
	}
	return false
}

// writeEventSSE serializes a single event row into the SSE framing
// the browser EventSource expects. Returns false on write failure so
// the caller can exit the loop. Uses event.Seq as the SSE id so the
// browser's automatic Last-Event-ID retry resumes cleanly.
func writeEventSSE(w io.Writer, ev store.Event) bool {
	type wire struct {
		RunID   string          `json:"run_id"`
		Seq     int64           `json:"seq"`
		NodeID  string          `json:"node_id,omitempty"`
		Kind    string          `json:"kind"`
		TS      time.Time       `json:"ts"`
		Payload json.RawMessage `json:"payload,omitempty"`
	}
	body, err := json.Marshal(wire{
		RunID:   ev.RunID,
		Seq:     ev.Seq,
		NodeID:  ev.NodeID,
		Kind:    ev.Kind,
		TS:      ev.TS,
		Payload: ev.Payload,
	})
	if err != nil {
		return false
	}
	frame := fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, body)
	_, werr := w.Write([]byte(frame))
	return werr == nil
}

func serveLogs(reader Reader, logSource LogSource, w http.ResponseWriter, r *http.Request, runID, nodeID string) {
	if nodeID != "" {
		content, err := logSource.ReadNode(r.Context(), runID, nodeID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if len(content) == 0 {
			return
		}
		_, _ = w.Write(content)
		return
	}

	// Whole-run: concatenate each node's log with a banner.
	nodes, err := reader.ListNodes(r.Context(), runID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for i, n := range nodes {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "=== %s (%s) ===\n", n.NodeID, n.Outcome)
		content, err := logSource.ReadNode(r.Context(), runID, n.NodeID)
		if err != nil {
			fmt.Fprintf(w, "(error: %v)\n", err)
			continue
		}
		_, _ = w.Write(content)
	}
}

// --- API wire types (JSON-serializable) ---

type apiRun struct {
	ID          string            `json:"id"`
	Pipeline    string            `json:"pipeline"`
	Status      string            `json:"status"`
	Trigger     string            `json:"trigger"`
	GitBranch   string            `json:"git_branch,omitempty"`
	GitSHA      string            `json:"git_sha,omitempty"`
	Repo        string            `json:"repo,omitempty"`
	RepoURL     string            `json:"repo_url,omitempty"`
	GithubOwner string            `json:"github_owner,omitempty"`
	GithubRepo  string            `json:"github_repo,omitempty"`
	RetryOf     string            `json:"retry_of,omitempty"`
	RetriedAs   string            `json:"retried_as,omitempty"`
	Args        map[string]string `json:"args,omitempty"`
	Error       string            `json:"error,omitempty"`
	StartedAt   time.Time         `json:"started_at"`
	FinishedAt  *time.Time        `json:"finished_at,omitempty"`
	DurationMs  int64             `json:"duration_ms"`
	Tags        []string          `json:"tags,omitempty"`
}

type apiNode struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	Outcome    string     `json:"outcome"`
	Deps       []string   `json:"deps"`
	Error      string     `json:"error,omitempty"`
	Output     any        `json:"output,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMs int64      `json:"duration_ms"`
	// Cluster-mode dispatch signal. Empty for laptop-local nodes.
	// Holder shapes: `pod:<runID>:<nodeID>` (K8sRunner fallback) or
	// `runner:<hostname>:<claim-nanos>` (warm pool).
	ClaimedBy      string     `json:"claimed_by,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	// StatusDetail is the runner-reported "currently doing X" string;
	// LastHeartbeat is the wall-clock of the runner's last ping.
	// Both feed the /pipelines summary activity row and HeartbeatDot.
	StatusDetail  string     `json:"status_detail,omitempty"`
	LastHeartbeat *time.Time `json:"last_heartbeat,omitempty"`
	// FailureReason keys the UI's FailureReasonBadge + help banner.
	// Empty for success / uncategorized failure. ExitCode carries the
	// runner-observed process exit code when one is available.
	FailureReason string `json:"failure_reason,omitempty"`
	ExitCode      *int   `json:"exit_code,omitempty"`
	// Groups carries every named *NodeGroup this node belongs to (declared
	// via sparkwing.Group(plan, name, members...)). Sourced from the plan
	// snapshot since store.Node itself has no group column. Empty for
	// ungrouped nodes; the dashboard renders those flat.
	Groups []string `json:"groups,omitempty"`
	// Dynamic marks nodes whose downstream shape is runtime-variable
	// — either explicit `.Dynamic()` or the source of an ExpandFrom.
	// The dashboard paints a rainbow "DYNAMIC" pill on these in the
	// DAG view, matching the terminal's rainbow-letter tag.
	Dynamic bool `json:"dynamic,omitempty"`
	// Approval marks nodes declared with plan.Approval(...). The
	// dashboard always renders an approval pill on these -- grey
	// before the gate is reached, amber-pulsing while pending, solid
	// green/red once resolved -- so the gate is visible throughout
	// the run, not only while awaiting a human.
	Approval bool `json:"approval,omitempty"`
	// OnFailureOf carries the parent node ID when this node was
	// attached via .OnFailure(id, job). The DAG uses it to draw a
	// dashed failure-branch edge and to place the recovery node in
	// a column right of its parent instead of stranding it at level 0.
	OnFailureOf string `json:"on_failure_of,omitempty"`
	// Modifiers carries the node's active Plan-layer modifiers so the
	// dashboard can render the dispatch envelope (Retry / Timeout /
	// RunsOn / Cache / Inline / hook presence) inline with the node
	// card. Sourced from the plan snapshot.
	Modifiers *snapshotNodeModifiers `json:"modifiers,omitempty"`
	// Work is the node's inner DAG: Steps with Needs plus SpawnNode
	// declarations. Populated for nodes registered via plan.Job.
	// Renderers walk this to draw the Plan -> Node -> Work -> Step
	// tree. Sourced from the plan snapshot.
	Work *snapshotNodeWork `json:"work,omitempty"`
}

func toAPIRun(r *store.Run, meta *pipelineMeta) apiRun {
	out := apiRun{
		ID:          r.ID,
		Pipeline:    r.Pipeline,
		Status:      r.Status,
		Trigger:     r.TriggerSource,
		GitBranch:   r.GitBranch,
		GitSHA:      r.GitSHA,
		Repo:        r.Repo,
		RepoURL:     r.RepoURL,
		GithubOwner: r.GithubOwner,
		GithubRepo:  r.GithubRepo,
		RetryOf:     r.RetryOf,
		RetriedAs:   r.RetriedAs,
		Args:        r.Args,
		Error:       r.Error,
		StartedAt:   r.StartedAt,
	}
	if r.FinishedAt != nil {
		out.FinishedAt = r.FinishedAt
		out.DurationMs = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
	}
	if meta != nil {
		out.Tags = meta.tags[r.Pipeline]
	}
	return out
}

func toAPIRuns(rs []*store.Run, meta *pipelineMeta) []apiRun {
	out := make([]apiRun, 0, len(rs))
	for _, r := range rs {
		out = append(out, toAPIRun(r, meta))
	}
	return out
}

func toAPINodes(ns []*store.Node, meta map[string]nodeMeta) []apiNode {
	out := make([]apiNode, 0, len(ns))
	for _, n := range ns {
		m := meta[n.NodeID]
		an := apiNode{
			ID:             n.NodeID,
			Status:         n.Status,
			Outcome:        n.Outcome,
			Deps:           n.Deps,
			Error:          n.Error,
			StartedAt:      n.StartedAt,
			FinishedAt:     n.FinishedAt,
			ClaimedBy:      n.ClaimedBy,
			LeaseExpiresAt: n.LeaseExpiresAt,
			StatusDetail:   n.StatusDetail,
			LastHeartbeat:  n.LastHeartbeat,
			FailureReason:  n.FailureReason,
			ExitCode:       n.ExitCode,
			Groups:         m.Groups,
			Dynamic:        m.Dynamic,
			Approval:       m.Approval,
			OnFailureOf:    m.OnFailureOf,
			Modifiers:      m.Modifiers,
			Work:           m.Work,
		}
		if n.StartedAt != nil && n.FinishedAt != nil {
			an.DurationMs = n.FinishedAt.Sub(*n.StartedAt).Milliseconds()
		}
		if len(n.Output) > 0 {
			var boxed any
			if err := json.Unmarshal(n.Output, &boxed); err == nil {
				an.Output = boxed
			}
		}
		if an.Deps == nil {
			an.Deps = []string{}
		}
		out = append(out, an)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
