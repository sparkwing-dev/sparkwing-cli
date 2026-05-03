package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkwing-dev/sparkwing-sdk/orchestrator"
	"github.com/sparkwing-dev/sparkwing-cli/internal/web"
	"github.com/sparkwing-dev/sparkwing-sdk/sparkwing"
)

// Pipelines used by the web-level tests. Registration is guarded to
// avoid duplicate panics across test invocations.
var registerOnce sync.Map

func register(name string, factory func() sparkwing.Pipeline[sparkwing.NoInputs]) {
	if _, loaded := registerOnce.LoadOrStore(name, struct{}{}); loaded {
		return
	}
	sparkwing.Register[sparkwing.NoInputs](name, factory)
}

type webOK struct{ sparkwing.Base }

func (webOK) Plan(_ context.Context, plan *sparkwing.Plan, _ sparkwing.NoInputs, rc sparkwing.RunContext) error {
	sparkwing.Job(plan, rc.Pipeline, sparkwing.JobFn(func(ctx context.Context) error {
		sparkwing.Info(ctx, "web hello")
		return nil
	}))
	return nil
}

type webDAG struct{ sparkwing.Base }

func (webDAG) Plan(ctx context.Context, plan *sparkwing.Plan, _ sparkwing.NoInputs, rc sparkwing.RunContext) error {
	a := sparkwing.Job(plan, "a", sparkwing.JobFn(func(ctx context.Context) error { return nil }))
	sparkwing.Job(plan, "b", sparkwing.JobFn(func(ctx context.Context) error { return nil })).Needs(a)
	return nil
}

func init() {
	register("web-ok", func() sparkwing.Pipeline[sparkwing.NoInputs] { return &webOK{} })
	register("web-dag", func() sparkwing.Pipeline[sparkwing.NoInputs] { return &webDAG{} })
}

// startServer spins up the web server on an ephemeral port and
// returns its base URL plus a cancel function.
func startServer(t *testing.T, paths orchestrator.Paths) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = web.Serve(ctx, paths, addr)
		close(done)
	}()

	base := fmt.Sprintf("http://%s", addr)
	// Wait for the server to accept connections.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(base + "/api/health"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return base, func() {
					cancel()
					<-done
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("web server did not become ready")
	return "", func() {}
}

func TestAPI_ListRunsEmpty(t *testing.T) {
	root := t.TempDir()
	paths := orchestrator.PathsAt(root)
	base, stop := startServer(t, paths)
	defer stop()

	body := mustGetJSON(t, base+"/api/runs")
	runs, _ := body["runs"].([]any)
	if len(runs) != 0 {
		t.Fatalf("expected empty runs, got %v", body)
	}
}

func TestAPI_RunDetailAndLogs(t *testing.T) {
	root := t.TempDir()
	paths := orchestrator.PathsAt(root)

	res, err := orchestrator.RunLocal(context.Background(), paths, orchestrator.Options{Pipeline: "web-ok"})
	if err != nil {
		t.Fatalf("orchestrator.Run: %v", err)
	}

	base, stop := startServer(t, paths)
	defer stop()

	// /api/runs shows the run.
	body := mustGetJSON(t, base+"/api/runs")
	runs, _ := body["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %v", body)
	}

	// /api/runs/<id> returns run + nodes.
	detail := mustGetJSON(t, base+"/api/runs/"+res.RunID)
	run, _ := detail["run"].(map[string]any)
	if run["id"] != res.RunID {
		t.Fatalf("run id mismatch: %v", run["id"])
	}
	if run["status"] != "success" {
		t.Fatalf("status = %v", run["status"])
	}
	nodes, _ := detail["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}

	// /api/runs/<id>/logs/<nodeID>
	logs := mustGetText(t, base+"/api/runs/"+res.RunID+"/logs/web-ok")
	if !strings.Contains(logs, "web hello") {
		t.Fatalf("node logs missing 'web hello': %q", logs)
	}

	// /api/runs/<id>/logs (whole run) includes the node banner + content.
	all := mustGetText(t, base+"/api/runs/"+res.RunID+"/logs")
	if !strings.Contains(all, "=== web-ok (success) ===") {
		t.Fatalf("whole-run logs missing banner: %q", all)
	}
	if !strings.Contains(all, "web hello") {
		t.Fatalf("whole-run logs missing content: %q", all)
	}
}

func TestAPI_DAGDepsSerialize(t *testing.T) {
	root := t.TempDir()
	paths := orchestrator.PathsAt(root)

	res, err := orchestrator.RunLocal(context.Background(), paths, orchestrator.Options{Pipeline: "web-dag"})
	if err != nil {
		t.Fatal(err)
	}

	base, stop := startServer(t, paths)
	defer stop()

	detail := mustGetJSON(t, base+"/api/runs/"+res.RunID)
	nodes, _ := detail["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	bNode := nodes[1].(map[string]any)
	deps, _ := bNode["deps"].([]any)
	if len(deps) != 1 || deps[0] != "a" {
		t.Fatalf("b deps = %v", deps)
	}
}

func TestAPI_StaticIndexServed(t *testing.T) {
	root := t.TempDir()
	paths := orchestrator.PathsAt(root)
	base, stop := startServer(t, paths)
	defer stop()

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// Next.js metadata emits "Sparkwing" (capital S) via layout.tsx.
	if !strings.Contains(string(body), "<title>Sparkwing</title>") {
		t.Fatalf("index missing expected title: %s", string(body))
	}
	// Go-side templating must leave the runtime markers consumable:
	// a literal empty-string substitution becomes `""`; the React
	// shim in api.ts then treats both the literal marker and the
	// empty string as "not configured".
	if !strings.Contains(string(body), `window.__SPARKWING_TOKEN__="";`) {
		t.Fatalf("token template not substituted in index")
	}
}

func TestAPI_PipelineFilter(t *testing.T) {
	root := t.TempDir()
	paths := orchestrator.PathsAt(root)

	_, _ = orchestrator.RunLocal(context.Background(), paths, orchestrator.Options{Pipeline: "web-ok"})
	_, _ = orchestrator.RunLocal(context.Background(), paths, orchestrator.Options{Pipeline: "web-dag"})

	base, stop := startServer(t, paths)
	defer stop()

	body := mustGetJSON(t, base+"/api/runs?pipeline=web-ok")
	runs, _ := body["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	first := runs[0].(map[string]any)
	if first["pipeline"] != "web-ok" {
		t.Fatalf("filter leaked: %v", first["pipeline"])
	}
}

func TestAPI_StatusFilter(t *testing.T) {
	root := t.TempDir()
	paths := orchestrator.PathsAt(root)

	_, _ = orchestrator.RunLocal(context.Background(), paths, orchestrator.Options{Pipeline: "web-ok"})

	base, stop := startServer(t, paths)
	defer stop()

	body := mustGetJSON(t, base+"/api/runs?status=failed")
	runs, _ := body["runs"].([]any)
	if len(runs) != 0 {
		t.Fatalf("expected 0 failed runs, got %d", len(runs))
	}
}

func TestAPI_UnknownRun404(t *testing.T) {
	root := t.TempDir()
	paths := orchestrator.PathsAt(root)
	base, stop := startServer(t, paths)
	defer stop()

	resp, err := http.Get(base + "/api/runs/run-does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func mustGetJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return body
}

func mustGetText(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}
