package web

import (
	"context"
	"errors"

	"github.com/sparkwing-dev/sparkwing-sdk/orchestrator/store"
)

// ErrCancelNotSupported is returned by readers that cannot cancel
// runs (e.g. local mode: there's no trigger queue to interrupt).
var ErrCancelNotSupported = errors.New("cancel not supported in local mode")

// Reader is the subset of store/controller-client methods the
// dashboard needs. Satisfied by both *store.Store (local mode) and
// *client.Client (cluster mode); lets the handlers stay ignorant of
// where the data came from.
type Reader interface {
	ListRuns(ctx context.Context, f store.RunFilter) ([]*store.Run, error)
	GetRun(ctx context.Context, runID string) (*store.Run, error)
	ListNodes(ctx context.Context, runID string) ([]*store.Node, error)
	CancelRun(ctx context.Context, runID string) error

	// Debug-pause surface (REG-013d). The dashboard lists pauses for a
	// run and releases individual nodes from the paused-state panel.
	ListDebugPauses(ctx context.Context, runID string) ([]*store.DebugPause, error)
	ReleaseDebugPause(ctx context.Context, runID, nodeID, releasedBy, kind string) error

	// ListEventsAfter returns the run's events with seq > afterSeq,
	// ordered by seq ascending. Backs the /api/runs/{id}/events/stream
	// SSE endpoint: the handler pages forward by feeding the last
	// delivered seq as the next afterSeq. Returns an empty slice (not
	// an error) when nothing new is available.
	ListEventsAfter(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.Event, error)
}

// storeReader adapts *store.Store to the Reader interface. Exists
// because store methods return *Run / *Node with the same shapes
// the controller's HTTP API serializes, so no translation is needed.
type storeReader struct {
	st *store.Store
}

func (r storeReader) ListRuns(ctx context.Context, f store.RunFilter) ([]*store.Run, error) {
	return r.st.ListRuns(ctx, f)
}
func (r storeReader) GetRun(ctx context.Context, id string) (*store.Run, error) {
	return r.st.GetRun(ctx, id)
}
func (r storeReader) ListNodes(ctx context.Context, runID string) ([]*store.Node, error) {
	return r.st.ListNodes(ctx, runID)
}
func (r storeReader) CancelRun(context.Context, string) error {
	return ErrCancelNotSupported
}
func (r storeReader) ListDebugPauses(ctx context.Context, runID string) ([]*store.DebugPause, error) {
	return r.st.ListDebugPauses(ctx, runID)
}
func (r storeReader) ReleaseDebugPause(ctx context.Context, runID, nodeID, releasedBy, kind string) error {
	return r.st.ReleaseDebugPause(ctx, runID, nodeID, releasedBy, kind)
}
func (r storeReader) ListEventsAfter(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.Event, error) {
	return r.st.ListEventsAfter(ctx, runID, afterSeq, limit)
}
