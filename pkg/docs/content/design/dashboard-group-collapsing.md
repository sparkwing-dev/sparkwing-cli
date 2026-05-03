# Dashboard: collapse nodes by plan.Group membership

Status: handoff. Estimated 2-4 hours for an agent with web/Next.js
familiarity.

## Goal

Members of a `plan.Group(name, ...)` declaration should render in the
pipelines page under a collapsible header that shows the group name
+ aggregate status. The membership data is already on every node
record — the dashboard just doesn't fully act on it yet.

## What's already in place

- `Plan.Group(name, members...) *Group` constructor — `pkg/sparkwing/combinator.go`
- `Plan.NodeGroupNames(id) []string` accessor — `pkg/sparkwing/plan.go`
- `run_plan` LogRecord includes `groups: ["..."]` per node — see
  `pkg/orchestrator/orchestrator.go` `emitRunPlan`
- The plan snapshot (`pkg/orchestrator/orchestrator.go`
  `marshalPlanSnapshot`) emits `groups []string` per node.
- `store.Node` DB row — check if it has a `group` column; if not
  and the dashboard needs it, add a migration.

## What to build

In `web/src/app/pipelines/page.tsx`, the middle column renders
`nodes` from `detail.nodes`. Today they render as a flat list
inside one `<NodesList>`. Change to:

1. Walk the nodes, partition by `node.groups[0]` (primary cluster).
   Nodes without any group memberships stay in an "ungrouped" bucket
   rendered at the top (current behavior preserved for pipelines that
   don't opt in).
2. For each group: render a `<GroupHeader>` with
   - Group name
   - Aggregate status pill (failed if any child failed, running if
     any child running, success if all done, pending otherwise)
   - Expand/collapse chevron (default expanded; persist
     collapsed-state in component state keyed by group name)
3. Indent child nodes under the group header.

Visual style: match the existing pipelines page aesthetic — dim
border, dim `(group name)` label. Don't add new libraries; use the
existing Tailwind utility classes and icons already imported.

## Where the group data lives

Two sources; prefer the one already populated:

- `detail.nodes[i].groups` — the backend exposes this on the Node
  API response. Check `apiNode` in `pkg/orchestrator/web/server.go`
  and `Node` in `web/src/lib/api.ts`.
- `detail.plan_snapshot` — the plan snapshot JSON, which embeds
  per-node metadata including `groups []string`. Use this if the
  Node API isn't enough.

## Verification

1. `wing example-release` — the `safety-checks` and `license-audit`
   nodes are members of `plan.Group("safety", ...)`. The dashboard
   pipelines page should render "safety" as a collapsible header
   with both nodes under it.
2. Click the header → nodes hide; click again → nodes show.
3. Group header shows green ✓ once both members complete.
4. Pipelines without any `plan.Group` declarations (today: most of
   them) render identically to today.

## Open questions (pick at implementation time)

- **Aggregate status when mixed:** failed > running > pending >
  success. Pick the highest-priority status across members.
- **Default collapsed or expanded?** Expanded while running,
  collapsed when all succeeded. Failed groups stay expanded so the
  failure is visible.
- **Multi-membership:** a node can belong to multiple named groups.
  For partitioning, anchor it to its first declared group; for
  bounding-box overlay, draw a frame for every group it belongs to.

## Files likely touched

- `web/src/app/pipelines/page.tsx` — new GroupHeader component,
  partition logic in the nodes list
- `web/src/lib/api.ts` — `groups: string[]` on Node already wired
- `pkg/orchestrator/web/server.go` — `apiNode.Groups []string`
  already wired

## Non-goals

- Cross-run group aggregation (e.g. "show all safety failures in
  the last 24h") — separate feature
- Nested groups — one level of grouping only
- Group-based filtering in the runs list — separate feature
