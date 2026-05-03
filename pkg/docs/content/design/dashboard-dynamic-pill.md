# Dashboard: rainbow [dynamic] pill for dynamic nodes

Status: handoff. ~1-2 hours for an agent with web/Next.js familiarity.

## Goal

Nodes that are `.Dynamic()` (or auto-inferred dynamic because they
source an `ExpandFrom`) should show a rainbow-gradient "DYNAMIC" pill
overlay in the dashboard — same signal the terminal's plan preview
renders as rainbow-letter text. Pure visual affordance so the reader
knows the node's shape is runtime-variable and the plan preview isn't
authoritative.

Reference inspiration: the "MOST TEAMS" pill on pricing-page-style
cards — a small corner badge.

## What's already in place

- `.Dynamic()` modifier on Node
- `Plan.IsDynamicNode(id)` (returns true for explicit + ExpandFrom sources)
- `run_plan` record carries `dynamic: true` per node
- Plan snapshot — verify this exposes `dynamic` on the per-node
  metadata; if not, add it
- Terminal renderer paints rainbow-letter `[dynamic]` tag — see
  `p.rainbow()` in `pkg/orchestrator/logger.go`

## What to build

1. Expose `dynamic` on the node API response. Check:
   - `apiNode` in `pkg/orchestrator/web/server.go` — add `Dynamic
     bool \`json:"dynamic,omitempty"\`` and populate from the plan
     snapshot or the in-memory plan.
   - `Node` type in `web/src/lib/api.ts` — add the field.
2. In the DAG view (`DAG` component in
   `web/src/app/pipelines/page.tsx`), when a node has `dynamic`
   true, render a small pill overlay in the corner of the node
   rectangle.
3. Style: conic-gradient or linear-gradient through the same hues
   the terminal uses (see `nodePalette` in
   `pkg/orchestrator/logger.go`: gold 214, sky 117, mint 114, hot
   pink 212, orange 208, lavender 141, terracotta 173, steel 109,
   violet 183, sea-green 115, salmon 174, periwinkle 147, dark
   yellow 178, sage 108, pink 176, red-orange 202). Pick a subset
   — 5-8 stops is enough for a pleasing gradient.
4. The pill label: "DYNAMIC" uppercase, small (10-11px), white
   text with a subtle drop shadow for readability against the
   gradient.

Suggested CSS:

```css
.dynamic-pill {
  position: absolute;
  top: -6px;
  right: -6px;
  padding: 2px 8px;
  border-radius: 999px;
  font-size: 10px;
  font-weight: 700;
  color: white;
  text-shadow: 0 0 4px rgba(0,0,0,0.5);
  background: linear-gradient(90deg,
    #ffaf00, #87d7ff, #87d787,
    #ff87d7, #ff8700, #af87d7);
}
```

Use Tailwind arbitrary properties or a scoped CSS file — match
whatever convention the rest of the pipelines page uses.

## Optional enhancements

- Animate the gradient (`background-size: 200% 100%; animation:
  dynamic-pulse 3s linear infinite;`) for a subtle shimmer. Check
  `prefers-reduced-motion` and disable the animation when set.
- Tooltip on hover: "This node may spawn additional work at
  runtime (ExpandFrom, AwaitPipelineJob, ...)."

## Verification

1. `wing example-release` — both `tests-harness` (ExpandFrom source,
   auto-inferred) and `post-deploy-followups` (explicit `.Dynamic()`)
   should render with the rainbow pill.
2. Non-dynamic nodes render normally, no pill.
3. Pill is readable on both dark and light terminal backgrounds.

## Files likely touched

- `web/src/app/pipelines/page.tsx` — DAG component
- `web/src/lib/api.ts` — Node type
- `pkg/orchestrator/web/server.go` — apiNode type (if needed)

## Non-goals

- Animated hue-shifting (keep it subtle; the gradient itself is
  the signal)
- Showing what work a dynamic node spawned (that's the job of the
  node's children in the DAG, already visible)
