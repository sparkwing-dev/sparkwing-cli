package web

import (
	"context"
	"io"
	"os"

	"github.com/sparkwing-dev/sparkwing-sdk/logs"
	"github.com/sparkwing-dev/sparkwing-sdk/orchestrator"
)

// LogSource fetches log contents for the dashboard. Local mode
// reads files under `paths.RunDir()`; cluster mode hits the
// sparkwing-logs service.
type LogSource interface {
	// ReadNode returns the log content for a single node, or
	// empty bytes (never an error) when logs aren't yet available.
	ReadNode(ctx context.Context, runID, nodeID string) ([]byte, error)

	// ReadRun returns concatenated banners+content for every node.
	// Used by the "all logs" dashboard view.
	ReadRun(ctx context.Context, runID string) ([]byte, error)

	// StreamNode opens a live-tail SSE stream for a node's log.
	// Returns nil, nil if the source doesn't support streaming
	// (e.g. diskLogSource: the dashboard falls back to polling).
	StreamNode(ctx context.Context, runID, nodeID string) (io.ReadCloser, error)
}

// diskLogSource is the local-mode implementation: reads directly
// from the worker's filesystem via orchestrator.Paths.
type diskLogSource struct {
	paths orchestrator.Paths
}

func (d diskLogSource) ReadNode(_ context.Context, runID, nodeID string) ([]byte, error) {
	f, err := os.Open(d.paths.NodeLog(runID, nodeID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (d diskLogSource) ReadRun(ctx context.Context, runID string) ([]byte, error) {
	// Delegate to the node-per-node handler; web's serveLogs does
	// the concatenation. This method exists for symmetry with the
	// HTTP source; wiring it into the handler is future work.
	return nil, nil
}

// StreamNode is unsupported for the disk source: the dashboard
// falls back to polled refresh. fsnotify on the file would give us
// local live-tail without a logs service, but that's a future
// polish -- cluster mode (httpLogSource) is where live tail
// actually matters.
func (d diskLogSource) StreamNode(context.Context, string, string) (io.ReadCloser, error) {
	return nil, nil
}

// httpLogSource fetches logs from the sparkwing-logs service. Used
// when `sparkwing web --logs-url URL` is set.
type httpLogSource struct {
	client *logs.Client
}

// NewHTTPLogSource constructs a LogSource that reads from the logs
// service at baseURL, authenticating every request with token when
// non-empty. The logs-service requires auth in cluster mode, so an
// empty token here produces silent "No logs captured" renders in
// the dashboard even though the logs are present on disk.
//
// The old single-arg constructor remains as a convenience wrapper
// so local-mode callers (no auth) don't have to pass an empty
// string explicitly.
func NewHTTPLogSource(baseURL, token string) LogSource {
	return httpLogSource{client: logs.NewClientWithToken(baseURL, nil, token)}
}

// NewHTTPLogSourceUnauthed is the unauthenticated form. Used by
// tests and local-mode dashboards where the logs service doesn't
// enforce auth.
func NewHTTPLogSourceUnauthed(baseURL string) LogSource {
	return httpLogSource{client: logs.NewClient(baseURL, nil)}
}

func (h httpLogSource) ReadNode(ctx context.Context, runID, nodeID string) ([]byte, error) {
	return h.client.Read(ctx, runID, nodeID)
}

func (h httpLogSource) ReadRun(ctx context.Context, runID string) ([]byte, error) {
	return h.client.ReadRun(ctx, runID)
}

func (h httpLogSource) StreamNode(ctx context.Context, runID, nodeID string) (io.ReadCloser, error) {
	return h.client.Stream(ctx, runID, nodeID)
}
