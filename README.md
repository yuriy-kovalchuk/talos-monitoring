# talos-monitoring

A Prometheus exporter and htmx dashboard for [Talos Linux](https://www.talos.dev)
node hardware and OS health. One pod, no DaemonSet, nothing installed on the
node.

## The problem

Talos nodes have no shell, no package manager, and nothing you can install on
the node — the OS is immutable and API-first by design. That rules out the
usual node-exporter model of a privileged DaemonSet reading `/proc` and `/sys`
on every node directly.

It also means most Talos fleets are missing signals node_exporter never had in
the first place: per-DIMM and per-socket inventory, PCI device inventory, Talos
service health, boot stage and readiness, volume encryption state, kernel
parameters vs. their defaults. On hardware with no BMC/IPMI — common on
mini-PCs and homelab gear — the OS is the only management interface there is,
and node_exporter can't reach it from outside the node.

## Why one pod, not a DaemonSet

Talos already exposes a gRPC machine API on every node (COSI resources plus
limited file reads), the same API `talosctl` uses from an operator's laptop,
authenticated in-cluster via a Kubernetes-native ServiceAccount mechanism.
Nothing has to run *on* the node to read it — one pod dials every node's API
directly (one client per node IP, `client.WithNode(ctx, ip)`).

So: a single Deployment, no privileged pod, no agent or `talosctl` binary to
install or upgrade per node.

## Design

```
Kubernetes API ──► node discovery ──► Talos API (per node, gRPC) ──► collectors
                                                                         │
                                                     ┌───────────────────┴───────────────────┐
                                                     ▼                                       ▼
                                        Prometheus sub-registry                     typed snapshot store
                                                     │                                       │
                                                     ▼                                       ▼
                                                /metrics                            htmx dashboard (/)
```

- A scheduler polls each collector on its own TTL (seconds for live data, an
  hour for inventory that rarely changes) and writes into both an exposition
  registry and an in-memory snapshot.
- `/metrics` and the dashboard are pure viewers of that state — reading either
  one never triggers a Talos API call.
- Read-only: the app only ever reads node state, never changes it.

Every exported metric is documented in [`docs/METRICS.md`](docs/METRICS.md).

## Prerequisites

### 1. Enable in-cluster Talos API access (all nodes)

The monitor authenticates to the Talos API in-cluster using Talos' built-in
service-account mechanism, which requires Talos API access for in-cluster apps
to be enabled in the machine configuration of **every node**:

```yaml
machine:
  features:
    kubernetesTalosAPIAccess:
      enabled: true
      allowedRoles:
        - os:reader           # roles the app may request (see note below)
      allowedKubernetesNamespaces:
        - talos-monitoring    # the namespace the app is deployed into
```

Apply it to the cluster, e.g.:

```console
$ talosctl patch machineconfig <<'EOF'
- op: add
  path: /machine/features/kubernetesTalosAPIAccess
  value:
    enabled: true
    allowedRoles: [os:reader]
    allowedKubernetesNamespaces: [talos-monitoring]
EOF
```

If Talos is installed via the [siderolabs/talos Helm chart](https://github.com/siderolabs/talos-charts),
put the same block under the chart's `machine.features` values instead (or in
addition), so the setting survives machine configuration regeneration.

> **Roles:** the app defaults to `os:reader`, enough for every COSI/machine-API
> collector. File-based collectors (sensors, hwmon, DMI, …) need the `Read`
> RPC, which is admin-only, and disable themselves gracefully without it. Set
> `talos.roles` (Helm value) and this machine config's `allowedRoles` to
> `[os:admin]` to enable those too — the app stays behaviorally read-only
> either way, but the `os:admin` credential itself is not.

### 2. Per-app Talos service account

A `talos.dev/v1alpha1` ServiceAccount in the app's namespace; Talos' in-cluster
controller turns it into a Kubernetes secret holding a short-lived (6h,
auto-renewed) Talos client certificate. The Helm chart creates this resource,
mounts the resulting secret, and sets `TALOSCONFIG` for the pod — no manual
step needed when installing via the chart.

## Running it

### Via Helm chart

```sh
helm install talos-monitoring oci://ghcr.io/yuriy-kovalchuk/charts/talos-monitoring \
  --version <version> \
  --namespace talos-monitoring --create-namespace
```

(add `helm registry login ghcr.io -u <github-username>` first if the package is
still private; the registry stays empty until the first `vX.Y.Z` tag is
pushed — until then, build and install from this checkout instead, see
[`examples/local-chart/`](examples/local-chart/))

Verify: the pod reaches 1/1 Ready. Until `kubernetesTalosAPIAccess` is enabled
on the nodes it stays 0/1 and logs `talosconfig unavailable, retrying` — that's
expected, not a crash.

Full deployment examples (released chart, and the local chart against a
self-built image) live in [`examples/`](examples/). Chart values are
documented in `charts/talos-monitoring/values.yaml` and validated by
`values.schema.json`.

### Locally

```sh
make build
KUBECONFIG=~/.kube/config TALOSCONFIG=~/.talos/config \
  ./bin/talos-monitoring serve --listen :8080
```

Outside Kubernetes the monitor picks up the usual config paths automatically
(`$KUBECONFIG`/`~/.kube/config`, `$TALOSCONFIG`/`~/.talos/config`). Run
`./bin/talos-monitoring serve --help` for the full flag list.

## Metrics & dashboard

Every metric is documented in [`docs/METRICS.md`](docs/METRICS.md), generated
from a live exposition and kept in sync with the code by a test. Open
`http://<host>:8080/` for the dashboard — a cluster overview, a per-node page,
and per-category pages (health, CPU, memory, storage, sensors, …). There are
no charts in the UI by design: it reports current state, and range queries
belong in Grafana against `/metrics` — dashboards for that ship in the Helm
chart.

## Security

The dashboard and `/metrics` are **unauthenticated** and expose hardware
identifiers (chassis serials, system UUIDs, DIMM serials, disk WWIDs) as
labels. The chart ships no NetworkPolicy — restricting network access to the
pod is the operator's responsibility.
