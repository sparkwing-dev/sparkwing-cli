package web

import (
	"net/http"
	"os"

	"github.com/sparkwing-dev/sparkwing-cli/pkg/pipelines"
)

// pipelinesPayload mirrors web/src/lib/api.ts:PipelineMeta so JSON
// round-trips into the TriggerForm.
type pipelinesPayload struct {
	Pipelines map[string]pipelineEntry `json:"pipelines"`
}

type pipelineEntry struct {
	Args []pipelineArg `json:"args"`
	Tags []string      `json:"tags,omitempty"`
}

type pipelineArg struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Desc     string `json:"desc"`
	Default  string `json:"default,omitempty"`
}

// pipelinesHandler serves the registered pipelines discovered from
// the nearest .sparkwing/pipelines.yaml. Args schemas are empty for
// v0 -- argument introspection lives in the compiled pipeline binary
// and plumbing that into the web process is a separate session.
func pipelinesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		payload := pipelinesPayload{Pipelines: map[string]pipelineEntry{}}
		cwd, err := os.Getwd()
		if err != nil {
			writeJSON(w, http.StatusOK, payload)
			return
		}
		_, cfg, err := pipelines.Discover(cwd)
		if err != nil {
			// Discovery failed -- the process isn't inside a
			// .sparkwing pipeline (e.g. prod dashboard pod). Return the
			// empty map; TriggerForm falls back to a free-text
			// pipeline name input.
			writeJSON(w, http.StatusOK, payload)
			return
		}
		for _, p := range cfg.Pipelines {
			payload.Pipelines[p.Name] = pipelineEntry{
				Args: []pipelineArg{},
				Tags: p.Tags,
			}
		}
		writeJSON(w, http.StatusOK, payload)
	}
}
