# Sparkwing Documentation

This directory contains user- and operator-facing documentation. The
web dashboard at `/docs` renders these pages.

**For SDK / pipeline-model internals, see `DESIGN-pipeline-model.md`
at the repo root - that is the authoritative reference for the
current SDK.** For overall project state and outstanding work, see
`STATE.md` at the repo root.

## Structure

```
docs/
  README.md              this file
  getting-started.md     install, quick start, run targets
  architecture.md        in-cluster deployment architecture
  cli.md                 wing + sparkwing CLI reference
  api.md                 controller HTTP API reference
  auth.md                principal + scope + argon2 token model
  security.md            transport, rate limiting, secret management
  deployment.md          deploy targets, gitops, ArgoCD, registries
  pipelines.md           pipeline YAML, jobs, modifiers
  hooks.md               triggers (webhooks + opt-in `pipeline hooks install`)
  caching.md             node-level CacheKey modifier
  build-caching.md       Docker / BuildKit / proxy caching layers
  fast-builds.md         performance best practices
  gitcache.md            sparkwing-cache: git HTTP, blobs, package proxy
  scheduling.md          runner labels, taints, tolerations, runs_on
  warm-pool.md           warm PVC pool
  mcp.md                 MCP server for AI agents
  sparks.md              spark library dependency management
  observability.md       failure reasons, resource metrics, OTel
  native-mode.md         the shipped laptop model (detached dashboard)
  local-execution.md     how local vs remote execution interact
  design/                feature design docs (approvals, SSE, etc.)
```

## What changed in the rewrite

See `STATE.md` at the repo root for the full story. Short version:

- The imperative-step SDK (`pkg/step`, `pkg/cache`, `sparkwing.Cmd[A,O]`,
  `RunStep`, `RunStepValue`, `Checks`, `SaveCache`/`RestoreCache`) was
  removed.
- Pipelines are now structs implementing `Plan(ctx, run) (*Plan, error)`
  -- one-step pipelines return a Plan with a single `Step`. Registered
  via `sparkwing.Register(name, factory)`.
- Bash chores (formatters, dev tasks) live in
  [dowing](https://github.com/koreyGambill/dowing); sparkwing is the
  Go-pipeline platform.
- Git-hook installation (`sparkwing pipeline hooks install`) is opt-in
  and writes managed pre-commit / pre-push hooks for pipelines that
  declare `pre_commit:` / `pre_push:` in their `on:` block.
- The CLI's top-level surface: `info`, `pipeline`, `run`, `runs`,
  `version`, `dashboard`, `cluster`, `secrets`, `configure`, `debug`,
  `docs`, `commands`, `completion`. (Cross-repo registry lives under
  `configure xrepo`; sparks library mgmt under `pipeline sparks`.)
- `pkg/localws` (the laptop dashboard server) is embedded in the
  sparkwing CLI and spawned as a detached process by `sparkwing
  dashboard start`. The standalone `sparkwing-local-ws` binary remains
  as an opt-in wrapper.
