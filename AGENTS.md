# AGENTS.md

**talos-monitoring** — a single Go binary that exports Talos node hardware as Prometheus
metrics and serves an htmx dashboard. Agentless: no DaemonSet, no privileged pod, nothing
runs on the node.

`docs/METRICS.md` is the metric reference; keep it in sync with the code — git history
covers completed work, so record decisions and constraints there, not a change log.

## Layout

```
cmd/talos-monitoring/   cobra CLI (serve)
internal/
  app/                  wiring: registry, 1s scheduler, HTTP server, 30s prune
  nodes/                k8s discovery + per-node Talos client pool
  collector/            Collector interface, Registry, Scraper, Gatherer
  collectors/           hwinfo, block, cpu, nodeapi, network, sensors, sysstat,
                        health, identity, timesync, kernel, volumes, gpu, diskio
  snapshot/             typed per-node inventory — the UI's data source
  history/              in-RAM ring, frequency samples only (the CPU page's observed range)
  metricguard/          metric-contract + §1.1 checks the collector tests run
  web/                  router, templates, rendering
static/                 embedded css, js (htmx, app.js), html, fonts
```

Data path: `Talos API → collector →` both `a prometheus sub-registry → /metrics` and
`snapshot.Store → web → HTML`. Two renderers, one collection. The UI never reads the
exposition format back, and neither `/metrics` nor a page render ever triggers an RPC.

## Invariants — do not break these

1. **Everything the UI shows must also be at `/metrics`.** The dashboard is a small view
   over a deliberately larger export. Exception: unbounded cardinality (per-PID) stays
   UI-only, which is also a reason not to build such a view.
2. **`/metrics` and the UI are pure viewers.** Only the scheduler collects.
3. **The scraper hands each collection a fresh sub-registry**, so anything cached
   internally must be re-registered every tick or the metric vanishes.
4. **All due collectors for a node run concurrently.** Shared state needs locking; shared
   RPCs need in-flight de-duplication (`collectors/sysstat` is the worked example).
5. **A published `*snapshot.Node` is immutable.** `Update` copies on write; readers hold
   it without a lock. Mutating in place is a data race on every dashboard request.
6. **Per-node state must implement `collector.Pruner`**, or it grows without bound on a
   cluster whose node names churn.
7. **Metric shape:** base units in the value slot, one family per unit, identifiers as
   labels never values, `_info` for identity and mutable attributes, `_total` only on
   counters. Full rules in `docs/METRICS.md`'s Conventions section.
8. **No inline `<script>` or `on*=` in templates** — the CSP is `script-src 'self'` and
   blocks them silently. `TestNoInlineScriptOrHandlers` catches it.
9. **A new collector wires `metricguard` into its test** — `Validate` for the metric
   contract and `SnapshotCovered` for §1.1. Without them a value can reach the UI that
   Prometheus never sees, which has already shipped once.
10. **Every metric needs a row in `docs/METRICS.md`** — `TestEveryMetricIsDocumented`
   fails on drift in either direction, including a backticked name in prose that is not
   a real family.
11. **`or` is not the template builtin** — it is a two-string helper named `fallback`.
   Using `{{if or .A .B}}` on booleans fails at render time with "wrong type for value";
   a page whose content block fails is answered with **500** (`TestRenderErrorReturns500`),
   never an empty 200. `TestNoChartMarkupRemains` and the per-page tests catch it.

## Talos specifics

- **Auth in-cluster:** `talos.dev/v1alpha1` ServiceAccount CR → controller-created Secret
  (key `config`, 6 h cert, auto-renewed) → `TALOSCONFIG`. Requires
  `machine.features.kubernetesTalosAPIAccess` in the machineconfig.
- **Dial per node.** COSI and file APIs are per-node; `talos.default.svc` is load-balanced
  and does not fan out. One client per node IP on `:50000`, routed with
  `client.WithNode(ctx, ip)`.
- **RBAC:** COSI and machine reads need `os:reader`; **file reads (`Read`) need
  `os:admin`**. File-based collectors self-disable on the first permission error, log a
  hint naming the Helm value, and count `err="permission"`.
- **Do not patch the user's cluster machineconfig without explicit permission.**

## Working here

- Go 1.27.x, Talos machinery v1.13.x (match the cluster), client-go v0.35.x,
  client_golang v1.24.x and htmx v2.0.9 (vendored). No charting library.
- Reference cluster: `KUBECONFIG=~/.kube/workload`, 4 nodes, Talos v1.13.4. Verify live —
  several bugs here were only caught by running against it, not by tests.
- Commits: `type: description`, no scopes. Enforced by `.githooks/commit-msg`.
- `make all` = fmt vet build test lint; `make test` runs `-race`.
- Tests assert behaviour, never UI copy. **When a test fails after a deliberate change,
  check whether it was pinning the bug** — several were.
