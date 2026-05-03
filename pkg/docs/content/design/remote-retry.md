# Remote retry: SpawnAll skipping passed siblings

Status: design / handoff. Local retry is implemented (`wing --retry-of <id>`
+ `sparkwing jobs retry -j <id>`); remote retry is the next step.

## Goal

`sparkwing jobs retry --on prod -j <parent-id>` should re-run a failed
cluster-dispatched pipeline and skip the spawn children that already
passed in the prior run — the same behavior that already works for
`SpawnAllLocal` in-process.

## Current state (what already exists)

- `Job.RetryOf string` exists in the controller and persists in the DB.
- `POST /jobs/{id}/retry` (controller `HandleRetry` in
  `internal/controller/retry.go`) enqueues a new job that copies the
  original's Pipeline / RepoURL / Branch / Env / ParentID / GitHub
  metadata. It does NOT currently set `RetryOf` on the new job (the
  `LinkRetry` call does, but that only wires the heartbeat linkage,
  not the env propagation we need).
- The agent runner (`internal/cli/agent.go` `executeJob`) executes the
  pipeline binary as a subprocess with env vars like
  `SPARKWING_PIPELINE`, `SPARKWING_REGISTRY`, `SPARKWING_ENVIRONMENT`.
  It does not propagate `SPARKWING_RETRY_OF`.
- `pkg/sparkwing.SpawnAllLocal` already reads `SPARKWING_RETRY_OF` and
  short-circuits children by reading
  `~/.sparkwing/runs/<retry-of>-*/run.json`. That file-based lookup
  only works locally.
- `pkg/sparkwing.SpawnAll` (the non-`Local` variant that dispatches
  children to the cluster via `/trigger`) has no retry awareness.

## Scope of the remote retry work

Three changes, all small:

### 1. Controller plumbs `retry_of` end-to-end

`HandleRetry` should set `RetryOf` on the enqueue opts, and `Enqueue`
should persist it on the new Job. The agent dispatcher (whatever
writes the runner pod's env) should surface `SPARKWING_RETRY_OF=<parent>`
when the Job has `RetryOf != ""`. Today the CLI's `wing` path sets
this env var via `RunOpts.RetryOf → SPARKWING_RETRY_OF`; the agent
needs to do the equivalent for pipelines it spawns.

Files to touch:
- `internal/controller/retry.go` — pass `RetryOf: jobID` in the
  `EnqueueOpts`.
- `internal/controller/queue.go` — `EnqueueOpts` gains a `RetryOf`
  field; `Enqueue` copies it onto the `Job`.
- `internal/cli/agent.go` — when launching the pipeline subprocess
  (around `runCmd := exec.CommandContext(...)` near line 514), if
  `job.RetryOf != ""`, append `SPARKWING_RETRY_OF=<retry-of>` to the
  env slice.

### 2. `SpawnAll` (client side) queries the controller for prior children

`SpawnAllLocal`'s retry lookup is filesystem-based. `SpawnAll` talks
to a controller and can't see `~/.sparkwing/runs/`. Instead it should:

1. Read `SPARKWING_RETRY_OF` from env at the start of the spawn.
2. If set, fetch the prior parent's children from the controller —
   a new or existing endpoint that returns the per-pipeline map of
   succeeded children. Two options for the endpoint:
   - Reuse `/jobs/{id}/tree` — returns children, filter client-side
     for status=complete.
   - Add a dedicated `/jobs/{id}/passed-children?by=pipeline` that
     returns `{"pipeline-name": "child-job-id"}`. More surgical.
3. Build a `map[string]bool` keyed by the spawn value (which is the
   child's `Pipeline` field), same shape as
   `priorPassedChildren()` in `pkg/sparkwing/childrun.go`.
4. For each value in the spawn, skip `/trigger` if the prior run had
   a passed child for it; synthesize a Result struct and return nil.

Files to touch:
- `pkg/sparkwing/spawn.go` — add retry short-circuit block in
  `SpawnAll` (not `SpawnAllLocal` — that already works).
- A new helper, probably in `pkg/sparkwing/childrun.go`, that hits
  the controller. Model after `priorPassedChildren`. It'll need the
  controller URL — the pipeline process already has
  `SPARKWING_CONTROLLER` in env, so use that.
- Controller handler: reuse `/jobs/{id}/tree` unless perf becomes a
  problem. The tree handler already joins children by parent_id.

### 3. CLI: remote path of `sparkwing jobs retry`

Already wired. `cli.RetryJob` (internal/cli/job_retry.go) calls
`POST /jobs/{id}/retry` — confirm it still works once #1 is in.

## Gotchas and design notes

- **Value-returning children** stay "always re-run". Do not attempt
  to cache return values remotely — leaking sensitive outputs through
  the job_cache table is a real concern and not worth the complexity
  for retry UX.
- **Children of children** (nested `SpawnAll` inside a spawn child):
  the grandchild's `retry_of` needs to reference the grandchild in
  the prior run, not the root. Mirror what local does with
  `withTrackedRunID(ctx, cr.id)` — the controller-side equivalent is
  that each spawned child's ParentID already points at its immediate
  parent. When the grandchild-parent retries, it sets
  `SPARKWING_RETRY_OF=<prior-grandchild-parent-id>` and the
  grandchild-parent's own `SpawnAll` call does the lookup against
  THAT id, not the root.
- **Cache keys** (the existing controller job_cache) are orthogonal
  to retry. A cached job still re-runs from the retry-of side —
  cache hit is about pipeline-input identity, retry is about
  parent-run identity.
- **Security**: `/jobs/{id}/tree` already requires auth (the same
  bearer token the CLI uses). No new auth surface.
- **Status matching**: use `StatusComplete` only, not `StatusCached`.
  Cached children produced their outputs from cache; they don't count
  as "passed work this run" — the retry should still re-use them
  because the cache layer sees the same inputs. Actually they can
  be treated identically: both "complete" and "cached" mean "don't
  re-run". Pick complete|cached as the skip set.

## Acceptance

- Trigger a failing remote pipeline that uses `SpawnAll` with several
  values where one fails.
- `sparkwing jobs retry --on prod -j <parent-id>` kicks a new
  run. Observe only the failed value re-dispatches; passed values
  synthesize cached results.
- `sparkwing jobs status --on prod -j <new-parent-id>` shows
  children where the cached ones have an appropriate marker
  (status="cached" or a new "skipped" status). Decide which — I'd
  say re-use the existing cached status since that's the same shape.

## Estimated work

Half a day, including the e2e test on prod.
