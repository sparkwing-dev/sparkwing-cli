# Approval gates: first-class manual approvals in the Plan

Status: design / handoff. Not yet implemented. Drafted as input for a
future session (human or agent) that takes this to completion.

## Goal

Pipeline authors should be able to insert a manual approval gate as a
first-class Plan primitive. An approved run proceeds; a denied run fails
with a clear outcome; a timed-out gate has configurable semantics.
Humans approve via the dashboard (click a button) or the CLI
(`sparkwing approve <run>/<node>`).

Not a plugin retrofit: the store, controller, orchestrator, CLI, and
dashboard all need awareness — same as any other node state.

## Non-goals (for v1)

- Multi-party approval (N-of-M, specific approvers). Single approver
  is enough for v1. Model future multi-party approvals as a separate
  `.ApproveBy(team)` layered on top.
- Approval policies beyond allow/deny (e.g. conditional auto-approve
  on main branch). Use `SkipIf` or a wrapping pipeline.
- Re-approval on retry. A retry that re-runs a gated node asks for
  approval again.
- Approval on cluster runners only. The feature works identically in
  laptop-local and cluster modes.

## Public surface

### SDK

```go
// Add a bare Approval gate.
approve := plan.Approval("approve-prod")
approve.Needs(integStg)
plan.Add("deploy-prod", &DeployJob{Env: "prod"}).Needs(approve)

// Or with options.
approve := plan.Approval("approve-prod",
    sparkwing.ApprovalMessage("Promote build %s to prod?", git.SHA),
    sparkwing.ApprovalTimeout(2*time.Hour),
    sparkwing.ApprovalOnTimeout(sparkwing.ApprovalDeny),
)
```

An Approval Node has no Job. The orchestrator treats it as a pause
point: dispatching the Node writes an `approval_pending` status,
records an `approval_requested` event with the message, and blocks
until either a release record arrives (approved / denied) or the
timeout fires.

`Approval` returns a `*Node` so all existing Node modifiers
(`Needs`, `SkipIf`, `OnFailure`) compose. The only modifier it
rejects is `.Inline()` — approvals are long-lived by design; they
should not tie up the dispatcher goroutine pool.

### Store (SQLite + controller)

New table `approvals` (migration added alongside the feature):

```
run_id         TEXT    NOT NULL
node_id        TEXT    NOT NULL
requested_at   INTEGER NOT NULL  -- unix millis
message        TEXT             -- operator-facing prompt
timeout_ms     INTEGER          -- 0 = never
on_timeout     TEXT             -- "deny" | "approve" | "fail" (default "fail")
approver       TEXT             -- set on resolution
resolved_at    INTEGER          -- unix millis, null while pending
resolution     TEXT             -- "approved" | "denied" | "timed_out"
comment        TEXT             -- optional operator-supplied note
PRIMARY KEY (run_id, node_id)
```

Node status values extended:

```
pending → approval_pending → (approved | denied | timed_out) → (success | failed | cancelled)
```

`approval_pending` is a new terminal status for the waiting state.
`approved` flips the node to `success` and propagates; `denied` and
`timed_out` (with `on_timeout=deny`) flip to `failed`; `timed_out`
with `on_timeout=approve` flips to `success`.

### Controller HTTP API

```
POST   /api/v1/runs/{run}/approvals/{node}
       body: {"resolution":"approved"|"denied","comment":"..."}
       returns: 200 with the resolved approval record

GET    /api/v1/runs/{run}/approvals
       returns: list of approval records for a run (pending + history)

GET    /api/v1/approvals/pending
       returns: list of pending approvals across all runs (for
       dashboard "awaiting approval" dropdown)
```

Auth: approvals use the same principal/scope machinery as every other
mutating endpoint. The `approver` field on the record is populated
from the authenticated principal, not the request body.

### Dashboard

Pipelines page — run detail: any node in `approval_pending` renders
with an indigo "awaiting approval" banner at the top of the log pane,
showing the prompt message, a multiline comment textarea, and two
buttons: `Approve` / `Deny`. Clicking either hits the POST endpoint.

Top-nav: a small badge next to the user menu showing the count of
pending approvals across all runs the current user can see. Opens a
dropdown listing them with one-click nav into each run.

### CLI

```
sparkwing approve <run-id>/<node-id>   [--comment "..."]
sparkwing deny    <run-id>/<node-id>   [--comment "..."]
sparkwing approvals list               [--on <profile>] [--run <id>]
```

`approve` and `deny` use the same profile resolution as other verbs
(`--on` or the default profile). They print the resolved record and
exit 0 on success, non-zero on "not pending" / "not authorized" / etc.

`approvals list` prints a table of pending approvals (table by
default, `-o json` for agents). Agents watching for a gate can poll
this endpoint.

## Orchestrator changes

### Dispatch

When an Approval Node becomes eligible (all deps satisfied), the
orchestrator:

1. `StateBackend.StartNode(run, node)` — existing.
2. `StateBackend.CreateApproval(run, node, message, timeout_ms,
   on_timeout)` — new. Inserts the `approvals` row and marks the
   node `approval_pending`.
3. `AppendEvent(run, node, "approval_requested", payload)` — new
   event type. Payload is `{"message":"...","timeout_ms":N}`.
4. Blocks on a poll loop (500ms) reading
   `StateBackend.GetApproval(run, node)` until `resolved_at != null`
   or ctx cancels.
5. On resolution: `FinishNode` with the appropriate outcome,
   `AppendEvent("approval_resolved", {resolution, approver, comment})`.

Timeout handling:
- Orchestrator process dies mid-wait → on restart it reconciles:
  loads pending approvals, resumes polling. No state loss.
- Timeout fires while the orchestrator is alive → the polling loop
  observes `now - requested_at > timeout_ms` and writes the timed_out
  resolution itself; human-issued approve/deny after that is a 409.

### Cycle detection & resume

Approval pauses count as "in flight" for cross-run cycle detection
(same as a node that's actually running). A second run of the same
pipeline while approval is pending is allowed; each has its own gate.

`sparkwing debug run --resume <run>` already handles node-state
resume for paused nodes; extend it to also recognize
`approval_pending` and poll for approval without restarting the
wait.

## Minimal implementation order (agent: do these in this order)

1. **Store schema.** Add `approvals` table + migration; add
   `approval_pending` / `approved` / `denied` / `timed_out` to the
   node-status enum. Write tests against `pkg/orchestrator/store`.
2. **StateBackend interface.** Add `CreateApproval`, `GetApproval`,
   `ResolveApproval`, `ListPendingApprovals` methods; implement on
   `localState` (SQLite) and `controllerClient` (HTTP). Stub the
   HTTP client call sites on the controller.
3. **Controller endpoints.** Add the three HTTP handlers, wire auth,
   write integration tests.
4. **SDK surface.** Add `plan.Approval(id, opts...)` + the
   `ApprovalOption` functional options in `pkg/sparkwing/plan.go`.
   Extend the Plan snapshot serialization so the dashboard can
   render approval nodes with their prompts.
5. **Dispatch.** New branch in `dispatchState.runOneNode`: if
   `node.IsApproval()`, go through the approval flow instead of
   `runner.RunNode`. Test in `pkg/orchestrator` with a fake state
   backend.
6. **CLI verbs.** `sparkwing approve` / `sparkwing deny` /
   `sparkwing approvals list` in `cmd/sparkwing/approvals.go`.
7. **Dashboard.** `ApprovalPane` component rendered inside the run
   detail when the selected node is in `approval_pending`. Top-nav
   badge in `Nav.tsx` hitting `GET /api/v1/approvals/pending`.
8. **Docs.** Update `docs/pipelines.md` with the `plan.Approval(...)`
   section; `docs/cli.md` with the new verbs.
9. **End-to-end verification.** `wing example-release` with the
   approve-prod node converted from the fake `ApproveJob` to a real
   `plan.Approval`. Resolve via dashboard + via CLI; confirm both
   propagate.

## Open questions (for the implementer to close)

1. **Resume semantics on orchestrator restart.** If the orchestrator
   crashes with an approval pending, does the waiter thread pick up
   where it left off, or does something else detect the orphan? In
   cluster mode the controller owns the run; this is simple. In
   laptop mode (`wing`) the pipeline binary dies with the terminal
   — user has to re-run. Document this limitation or fix it by
   making laptop mode also able to reattach to a pending approval
   via a persistent waiter in the local web server (`pkg/localws`,
   embedded in the sparkwing CLI).
2. **Scope for who can approve.** v1: any authenticated principal
   with scope `approvals:write`. Future: per-pipeline approver
   lists.
3. **Event stream.** Should the dashboard's SSE log stream include
   `approval_requested` / `approval_resolved` as special-event
   records so the live log viewer can render an approval banner
   without a full refresh? Yes — reuse the `LogRecord.Event` taxonomy
   with two new kinds.
4. **OnTimeout default.** Today I've drafted `fail`. Alternative:
   `deny` (treat no-answer as "no"). Pick based on user preference —
   `fail` feels more honest ("something went wrong, the gate wasn't
   answered") while `deny` is more conservative. Operator can always
   override per-Approval.
5. **Cross-run cascades.** If approval-gated run A spawned child run
   B via `AwaitPipelineJob`, and A is denied — should B get
   cancelled? Probably yes, but that's general run cancellation and
   not specific to approvals.

## Files likely to change

- `pkg/orchestrator/store/store.go` — migration, types, queries
- `pkg/orchestrator/backends.go` — StateBackend interface methods
- `pkg/orchestrator/http{state,logs}.go` — HTTP client implementations
- `pkg/controller/handlers.go` — three new endpoints
- `pkg/controller/server.go` — route wiring
- `pkg/sparkwing/plan.go` — `Approval`, options, new Node flag
- `pkg/orchestrator/orchestrator.go` — dispatch branch + poll loop
- `pkg/orchestrator/inprocess_runner.go` — approval-aware skip path
- `cmd/sparkwing/approvals.go` — new subcommand file
- `cmd/sparkwing/main.go` — register subcommand
- `web/src/app/pipelines/page.tsx` — `ApprovalPane` + pending badge
- `web/src/components/Nav.tsx` — pending badge + dropdown
- `web/src/lib/api.ts` — client functions
- `docs/pipelines.md`, `docs/cli.md`, `.sparkwing/jobs/example_release.go`
  (swap fake `ApproveJob` for `plan.Approval`)

## Rough effort estimate

- Store + backend + controller: ~1 day
- SDK + dispatch + tests: ~1 day
- CLI: half a day
- Dashboard: ~1 day
- Docs + e2e verify: half a day

Total: **3-4 days** for a senior agent working single-threaded, plus
review time. Not a weekend hack — do it properly.
