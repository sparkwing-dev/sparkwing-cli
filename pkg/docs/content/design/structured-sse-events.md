# Structured SSE events for live dashboard updates

Status: handoff. Estimated 1-2 days — cross-cutting change touching
server + client + log pipeline.

## Goal

The dashboard today polls `GET /api/runs/{id}` every 2s to pick up
state changes. That's fine at 2s resolution, but for approval-gate
handling (user clicks Approve → orchestrator waits up to 500ms →
polling loop picks it up → UI polls up to 2s → users see ~2.5s
latency) and for step-level log structure, it's coarse.

Replace the polling with Server-Sent Events carrying the structured
records we already emit to disk (node_start, node_end, step,
approval_requested, approval_resolved, run_summary, etc.). Clients
subscribe once and react instantly.

## What's already in place

- Per-node log streams via SSE — see
  `pkg/orchestrator/web/server.go` `serveLogStream` which pipes
  the logs-service stream for one node.
- Structured `LogRecord` JSONL format on disk
- Event types defined in `LogRecord.Event` (node_start, node_end,
  step, retry, exec_line, run_plan, run_summary,
  approval_requested, approval_resolved, plan_warn)
- `store.Event` rows in the events table (node_started,
  node_failed, cache_hit, approval_requested, etc.) — the
  controller already writes these; they're not currently streamed

## What to build

### Backend: `/api/runs/{id}/events/stream`

New SSE endpoint on the pipelines server
(`pkg/orchestrator/web/server.go`) that:

1. Subscribes to the store's events table for this run — tailing
   with a polling loop OR (preferable) a pub/sub bus if you add
   one. Start with polling at 250ms since it's a per-subscriber
   cost.
2. Emits each event as an SSE message: `event: <kind>\ndata:
   <json>\n\n`.
3. Closes when the run reaches a terminal status and all pending
   events drain.
4. Honors `Last-Event-ID` for client reconnection (server reads
   the header, resumes from the event ID that follows).

Event kinds to emit (from what's already in the events table):
- `node_started` / `node_succeeded` / `node_failed` /
  `node_skipped` / `node_cancelled`
- `cache_hit`
- `attempt_retry`
- `approval_requested` / `approval_resolved`
- `run_summary` (synthesized on run finish — payload: the full
  summary we already compute)

### Client: `useRunEvents` hook

New hook in `web/src/lib/useRunEvents.ts`:

```ts
export function useRunEvents(runID: string | null) {
  const [events, setEvents] = useState<Event[]>([]);
  useEffect(() => {
    if (!runID) return;
    const es = new EventSource(`/api/runs/${runID}/events/stream`);
    es.addEventListener("node_started", (e) => ...);
    es.addEventListener("approval_requested", (e) => setEvents(prev => [...prev, parse(e)]));
    // ... one listener per event kind
    return () => es.close();
  }, [runID]);
  return events;
}
```

### Client: replace polling in PipelinesPage

In `web/src/app/pipelines/page.tsx`:

- Replace the `setInterval(loadDetail, POLL_MS)` with a subscription
  via the new hook.
- Keep one initial `getRun` fetch on run selection to populate the
  baseline state; then mutate state in response to events.
- Keep polling as a fallback if the EventSource errors out.

Also: the `ApprovalPane` currently waits for the next poll cycle
after the user clicks Approve. With event streaming, resolving
the approval triggers an `approval_resolved` event that the hook
picks up in real time; the pane hides as soon as the event lands.

## Schema: event payloads

Every SSE event has a stable JSON shape so the client parser can
stay simple:

```json
{
  "run_id": "...",
  "node_id": "...",      // omitted for run-level events
  "kind": "node_started",
  "ts": "2026-04-23T...",
  "attrs": { ... }       // kind-specific
}
```

Match what's already in `store.Event` as closely as possible so the
server doesn't have to rewrite records.

## Verification

1. `wing example-release` — open the dashboard, watch nodes flip
   from pending→running→success with no perceptible polling delay.
2. Trigger a run with `--fail-mode deploy-staging`. The rollback
   node should animate in the moment OnFailure fires.
3. Approval gate: click Approve, confirm the pane hides and
   deploy-prod's row flips to running within ~300ms.
4. Reconnect test: kill the SSE connection mid-run; confirm the
   client reconnects and resumes where it left off (no dropped
   events, no duplicates).

## Tradeoffs / considerations

- **Polling fallback** is important. If the network drops SSE for
  >N seconds, fall back to the existing `getRun` polling so users
  on flaky networks don't silently fall behind.
- **Subscription fan-out**: 100 concurrent dashboards subscribing
  to different runs = 100 goroutines polling the events table.
  Acceptable at our scale; revisit with a single events-tailing
  goroutine that broadcasts to subscribers when we scale up.
- **Auth**: SSE uses the same cookie / principal as other API
  endpoints. Don't skip auth because EventSource can't set
  Authorization headers — the cookie path is sufficient.

## Files likely touched

- `pkg/orchestrator/web/server.go` — new handler + route
- `pkg/orchestrator/web/logs_source.go` — may reuse the
  polling-events pattern
- `pkg/orchestrator/store/store.go` — ensure events table has a
  monotonic ID (probably already does — used by Last-Event-ID)
- `web/src/lib/useRunEvents.ts` — new file
- `web/src/app/pipelines/page.tsx` — subscribe, mutate state
- `web/src/components/ApprovalPane.tsx` — trigger refetch on
  approval_resolved (or hide optimistically)

## Non-goals (v1)

- Global events stream ("show me every approval across all runs in
  real time"). The pending-approvals badge in the nav can keep
  polling at 5s for now.
- Server push beyond SSE (no WebSockets, no gRPC streaming).
- Backfilling historical events into a finished run's event stream
  (that's just loading the events table — no need to stream).
