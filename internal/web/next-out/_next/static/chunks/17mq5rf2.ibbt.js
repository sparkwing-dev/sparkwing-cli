(globalThis.TURBOPACK||(globalThis.TURBOPACK=[])).push(["object"==typeof document?document.currentScript:void 0,37153,e=>{"use strict";var r=e.i(43476);e.s(["default",0,function(){return(0,r.jsxs)("div",{className:"flex-1 overflow-y-auto",children:[(0,r.jsxs)("div",{className:"bg-gradient-to-b from-indigo-500/10 to-transparent px-8 py-16 text-center",children:[(0,r.jsxs)("h1",{className:"text-4xl font-bold mb-4",children:["CI/CD pipelines in ",(0,r.jsx)("span",{className:"text-indigo-400",children:"Go"})]}),(0,r.jsx)("p",{className:"text-lg text-[var(--muted)] max-w-2xl mx-auto mb-8",children:"Write your pipelines as real code, not YAML. Get compile-time errors, IDE autocomplete, and the full power of a real programming language for your CI/CD."}),(0,r.jsx)("div",{className:"flex gap-4 justify-center",children:(0,r.jsx)("code",{className:"bg-[var(--surface)] border border-[var(--border)] px-4 py-2 rounded-lg text-sm font-mono",children:"sparkwing cluster create --name dev"})})]}),(0,r.jsxs)("div",{className:"max-w-5xl mx-auto px-8 pb-16",children:[(0,r.jsxs)("section",{className:"py-12",children:[(0,r.jsx)("h2",{className:"text-2xl font-bold mb-8 text-center",children:"Why Sparkwing"}),(0,r.jsx)("div",{className:"grid grid-cols-1 md:grid-cols-3 gap-6",children:[{title:"Code, not config",desc:"Your pipelines are Go programs. Real imports, real functions, real error handling. No YAML templating nightmares.",color:"text-indigo-400"},{title:"One command setup",desc:"sparkwing cluster create gives you a full CI/CD stack in 2 minutes. Controller, listeners, dashboard, git cache — all local.",color:"text-green-400"},{title:"Content-addressed caching",desc:"Same code = same hash = skip. Tests that already passed? Instant. Builds that already ran? Cached. Zero wasted compute.",color:"text-cyan-400"},{title:"Structured pipelines",desc:"Jobs run sequentially. Parallel() runs jobs concurrently. Breakpoints let you pause and inspect. Timeouts keep things safe.",color:"text-violet-400"},{title:"GitHub native",desc:"Push to a branch → status check on your PR. Per-job reporting. Webhook-triggered. Branch enforcement for deploys.",color:"text-amber-400"},{title:"Secure by default",desc:"Deploy requirements, signed cache entries, rate limiting, audit logging, API auth. Trust verification before every deploy.",color:"text-red-400"}].map(e=>(0,r.jsxs)("div",{className:"bg-[var(--surface)] border border-[var(--border)] rounded-lg p-6",children:[(0,r.jsx)("h3",{className:`font-bold mb-2 ${e.color}`,children:e.title}),(0,r.jsx)("p",{className:"text-sm text-[var(--muted)]",children:e.desc})]},e.title))})]}),(0,r.jsxs)("section",{className:"py-12 border-t border-[var(--border)]",children:[(0,r.jsx)("h2",{className:"text-2xl font-bold mb-8 text-center",children:"How Sparkwing Compares"}),(0,r.jsx)("div",{className:"overflow-x-auto",children:(0,r.jsxs)("table",{className:"w-full text-sm",children:[(0,r.jsx)("thead",{children:(0,r.jsxs)("tr",{className:"border-b border-[var(--border)] text-[var(--muted)]",children:[(0,r.jsx)("th",{className:"px-4 py-3 text-left"}),(0,r.jsx)("th",{className:"px-4 py-3 text-left text-indigo-400",children:"Sparkwing"}),(0,r.jsx)("th",{className:"px-4 py-3 text-left",children:"GitHub Actions"}),(0,r.jsx)("th",{className:"px-4 py-3 text-left",children:"Jenkins"}),(0,r.jsx)("th",{className:"px-4 py-3 text-left",children:"Buildkite"})]})}),(0,r.jsx)("tbody",{children:[["Pipeline language","Go (compiled)","YAML","Groovy DSL","YAML + bash"],["Compile-time errors","Yes","No","No","No"],["Self-hosted","Yes (one command)","No","Yes (complex)","Agents only"],["Local dev works","Full stack local","No","No","No"],["Job caching","Content-addressed","Manual","Plugin","Manual"],["Breakpoints","Yes (.BreakBefore())","No","No","No"],["Live log streaming","SSE built-in","Built-in","Plugin","Built-in"],["Per-job GitHub status","Automatic","Per-job","Plugin","Plugin"],["Pipeline as code","Go imports","YAML","Jenkinsfile","YAML + plugins"],["Setup time","2 minutes","N/A (SaaS)","Weekend","30 minutes"]].map(([e,...t])=>(0,r.jsxs)("tr",{className:"border-b border-[var(--border)]",children:[(0,r.jsx)("td",{className:"px-4 py-2 font-medium",children:e}),t.map((e,t)=>(0,r.jsx)("td",{className:`px-4 py-2 ${0===t?"text-indigo-400 font-medium":"text-[var(--muted)]"}`,children:e},t))]},e))})]})})]}),(0,r.jsxs)("section",{className:"py-12 border-t border-[var(--border)]",children:[(0,r.jsx)("h2",{className:"text-2xl font-bold mb-8 text-center",children:"Real Examples"}),(0,r.jsxs)("div",{className:"space-y-8",children:[(0,r.jsxs)("div",{children:[(0,r.jsx)("h3",{className:"font-bold mb-3",children:"Simple build + deploy"}),(0,r.jsx)("pre",{className:"bg-[#0d1117] border border-[var(--border)] rounded-lg p-4 text-sm font-mono overflow-x-auto",children:`package jobs

import (
    "github.com/koreyGambill/sparks-core/docker"
    "github.com/koreyGambill/sparks-core/deploy"
)

func JobMyapp() {
    docker.BuildAndPush(docker.BuildConfig{
        Image:      "myapp",
        Dockerfile: "Dockerfile",
        Registries: registries,
    })
    deploy.Run(deploy.Config{
        AppName:   "myapp",
        Namespace: "default",
        Images:    []string{"myapp"},
    })
}`})]}),(0,r.jsxs)("div",{children:[(0,r.jsx)("h3",{className:"font-bold mb-3",children:"Parallel test shards + deploy"}),(0,r.jsx)("pre",{className:"bg-[#0d1117] border border-[var(--border)] rounded-lg p-4 text-sm font-mono overflow-x-auto",children:`func JobMyapp() {
    // Run tests in parallel shards
    sparkwing.SpawnAll("test", []string{"0", "1", "2"},
        sparkwing.SpawnPipeline("myapp-test"),
        sparkwing.WithEnv(func(val string) map[string]string {
            return map[string]string{"SHARD": val}
        }),
    )

    // Lint in parallel via a named step
    sparkwing.RunStep(step.Shell("lint", "golangci-lint run"))

    // Build and push
    docker.BuildAndPush(docker.BuildConfig{
        Image:      "myapp",
        Dockerfile: "Dockerfile",
        Registries: registries,
    })

    // Gate deploy behind approval
    sparkwing.RunStep(step.RequireApproval("deploy-gate"))
    deploy.Run(deploy.Config{
        AppName:   "myapp",
        Namespace: "prod",
        Images:    []string{"myapp"},
    })
}`})]}),(0,r.jsxs)("div",{children:[(0,r.jsx)("h3",{className:"font-bold mb-3",children:"Dynamic monorepo detection"}),(0,r.jsx)("pre",{className:"bg-[#0d1117] border border-[var(--border)] rounded-lg p-4 text-sm font-mono overflow-x-auto",children:`// Reads SPARKWING_CHANGED_FILES from the webhook
// Only builds apps whose files actually changed
func JobBuildChanged() {
    apps := changedApps()
    sparkwing.SpawnAll("build-changed", apps,
        sparkwing.SpawnPipeline("build-app"),
        sparkwing.WithEnv(func(app string) map[string]string {
            return map[string]string{"APP": app}
        }),
    )
}`})]})]})]}),(0,r.jsxs)("section",{className:"py-12 border-t border-[var(--border)]",children:[(0,r.jsx)("h2",{className:"text-2xl font-bold mb-8 text-center",children:"Every Feature"}),(0,r.jsx)("div",{className:"grid grid-cols-1 md:grid-cols-2 gap-4 text-sm",children:["Pipeline framework (Pipeline/Job/Parallel/Post)","Parallel job execution","Job timeouts","Pipeline breakpoints (.BreakBefore())","Content-addressed job caching","Real-time log streaming (SSE)","Job cancellation","Failed job retry","GitHub webhook integration","Per-job GitHub commit statuses","Webhook signature verification","Deploy branch enforcement","Test verification before deploy","Rate limiting on trigger API","Audit logging (SQLite)","HMAC-signed cache entries","API bearer token auth","Cron/scheduled builds","Multi-repo support","Artifact upload/download","Build matrix (axis combinations)","Concurrency controls (group limits)","Shell/Script/Named steps","Hooks lifecycle (5 points)","Plugin system (Go modules)","Git cache service (tarball serving)","Git worktrees for local runs","Local spawn (no cluster needed)","Multi-cluster management","Dynamic port allocation","Per-workflow directories","Metrics endpoint","Flaky test detection","Coverage reporting","Web dashboard with pipeline viz","Live log viewer","Agent capacity monitoring","Debug attach/env/continue"].map(e=>(0,r.jsxs)("div",{className:"flex items-center gap-2 px-3 py-2 bg-[var(--surface)] rounded",children:[(0,r.jsx)("span",{className:"text-green-400 shrink-0",children:"✓"}),(0,r.jsx)("span",{children:e})]},e))})]})]})]})}])}]);