# sparkwing-cli

Operator + laptop tooling for [sparkwing](https://github.com/sparkwing-dev/sparkwing-sdk).

## Binaries

- `sparkwing` — operator/dev CLI: scaffolds `.sparkwing/` repos, runs
  pipelines locally or against a remote cluster, manages secrets,
  inspects run history, runs the local dashboard.
- `sparkwing-local-ws` — laptop dashboard server (thin wrapper).
- `sparkwing-web` — dashboard binary; runs in laptop mode (reads local
  SQLite + log files) or cluster mode (proxies to a remote controller +
  logs service).

## Status

Private during initial development. Will be flipped public once the
sparkwing-sdk surface settles.

## Repo relationships

- Imports [sparkwing-sdk](https://github.com/sparkwing-dev/sparkwing-sdk)
  for pipeline types and runtime.
- Currently requires the sparkwing engine repo because `pkg/localws`
  embeds the controller server in-process. This dependency is reviewed
  before any future public flip.
