# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] - 2026-11-01

### Added
- **etcd collector** for control-plane members, through the Talos API (`os:reader` is enough, no etcd metrics listener): leader and quorum (`talos_etcd_has_leader`, `talos_etcd_is_leader`), raft term and apply progress, database size and in-use bytes, member count, learner state, and `NOSPACE`/`CORRUPT` alarms exported 0/1 so alerts never arm on absent data. Workers are never queried and carry no etcd series. The node health page shows an etcd card on control planes. Toggle with `--collectors.etcd.enabled` / Helm `collectors.etcd.enabled`
- **GPU identity without `os:admin`**: every display-class PCI device is reported from the COSI inventory, with its DRM card and the driver bound *now* resolved from `LS` symlink targets. `talos_gpu_info` and `talos_gpus` exist on `os:reader`; only utilisation and VRAM still need file reads
- `make run TALOSCONFIG=… KUBECONFIG=… LISTEN=…` to run locally against any talosconfig, including an `os:admin` one
- `make install-hooks`, a `pre-push` hook (commit subjects on `main`, `v*` tag format, race tests) and a 72-character subject limit in `commit-msg`
- Dependabot for Go modules, GitHub Actions and Docker images; CI builds the image on every pull request without publishing it

### Changed
- **Dashboard redesign** in the Google Cloud console style, with light and dark themes: the theme follows the OS by default and a top-bar switch cycles system, light and dark (remembered per browser). Navigation drawer with icons, a shared header on every node page (roles, address, Talos and Kubernetes versions), related cards paired side by side, and Roboto / Roboto Mono in place of Manrope / JetBrains Mono; help and empty-state texts are shorter. Navigation is grouped Monitor / Hardware / Software with the node picker in the drawer, and each page leads with live tiles and meters: services as a status grid, memory modules and network interfaces as cards, filesystems and PersistentVolumes as usage bars
- **Network throughput needs only `os:reader`**: the counters come from the `NetworkDeviceStats` RPC instead of reading `/proc/net/dev`, so the `network` collector no longer reports `collector_degraded{reason="permission"}`. A failed stats call now fails the collection and is counted like any other RPC error
- **Permission-gated reads recover without a restart**: a refused file read pauses that collector's reads for 5 minutes and probes again, instead of disabling them until the process restarts
- **`talos_gpus` counts GPU hardware**, driver or not (an undriven iGPU now counts 1); a DRM card with no PCI device behind it (`simpledrm`'s boot framebuffer) is not a GPU. `pci_id` is lowercase
- **`talos_kernel_module_info{state}` is lowercase** (`live`, was `Live`): Talos v1.14 reports modules as `KernelModuleStatus`; built-ins are dropped so the count is unchanged, and pre-v1.14 nodes fall back to `LoadedKernelModule`
- Toolchain and dependencies: Go 1.27.3, Talos machinery v1.14.5, cosi runtime v1.16.4, client-go v0.37.2, client_golang v1.25.0, grpc v1.85.0, htmx 2.0.11, golangci-lint v2.15.0, Helm v4.3.1, distroless `static-debian13`, latest SHA-pinned Actions. `k8s.io/kube-openapi` stays at the revision apimachinery v0.37 requires
- CD runs the full CI checks before publishing a release; the CI token defaults to read-only
- Screenshots retaken for the redesign, light and dark, under `docs/assets/ui/` and `docs/assets/grafana/`; the README shows a few and its intro is reworded
- The README and chart notes describe the `KubeTalosAPIAccessConfig` machine-config document as the prerequisite, with the deprecated `machine.features.kubernetesTalosAPIAccess` kept for older Talos
- `--inventory-interval` help says the flag is currently unused (every inventory collector declares its own 1h TTL)
- Published images carry OCI labels (`org.opencontainers.image.source` links the GHCR package to the repository), with tags from `docker/metadata-action`. CI runs gofmt through golangci-lint instead of separate vet and format steps
- **Images are multi-arch** (`linux/amd64` and `linux/arm64`, cross-compiled), **signed** with keyless cosign, and carry SLSA provenance and an SBOM. A fixable CRITICAL/HIGH vulnerability (Trivy) blocks the push, and PRs build and scan the image without publishing
- **Chart versioning follows `Chart.yaml`**: `version` and `appVersion` are the release without the `v` (`0.1.1`), set in a release-prep commit; CD refuses a tag that does not match them or has no `CHANGELOG.md` section, instead of stamping the chart from the tag. Release images are tagged `:X.Y.Z` (what `appVersion` points at) as well as `:vX.Y.Z`, and the GitHub Release notes are the tag's changelog section
- CI hardening: downloaded tools are verified against pinned sha256 sums, govulncheck is pinned, every job has a timeout, superseded PR runs are cancelled but `main` and release runs always finish

### Fixed
- **Grafana dashboards**, checked panel by panel against live data:
  - System: every service panel (stat, table, failing/stopped/no-check counts, timeline) aggregated the whole fleet instead of the selected node
  - Sensors and GPU: two chips with the same driver name (two amdgpu cards) merged into one series, so power, temperatures and voltages dropped a card; GPU power summed one card, GTT capacity was counted once per card
  - Overview: fleet cores, threads, memory and DIMMs double-counted while two exporter pods overlapped (every rollout), and empty memory slots counted as DIMMs
  - Network: MTU read "1 KiB", throughput totals re-counted tunnel traffic; throughput is now in bit/s like link speed
  - PCI: link speed read "16000000000 T/s" (now GT/s), hex vendor/device IDs rendered as numbers ("4.13 K"), a reference-counted enable read "2"
  - Storage: utilisation could read 2,700% after a restart; loop devices and always-ready pseudo-volumes are hidden; the rotational flag no longer claims SSD/HDD
  - Memory: committed memory is shown in bytes, not as a percentage of a limit only enforced with `vm.overcommit_memory=2`; kernel tunables read 7200, not "7.20 K"
  - Every legend fits its panel, CPU cores sort numerically, the run queue is labelled runnable, and empty state panels say what they mean instead of "No data"
- Network throughput double-counted tunnel traffic once WireGuard links read as up: totals now sum physical NICs only, which every byte on the wire crosses. Throughput is shown in bit/s, the unit of the link speed beside it, with each link's utilisation
- The dashboard showed an NVMe drive's unset-threshold sentinel (65261.8 °C) as a limit that no metric carried; the snapshot now drops it like the metric does
- Empty memory slots (size 0) counted as installed DIMMs, so a node with 2 of 4 slots populated reported 4; empty slots now read "empty"
- A GPU's power cap is shown as the limit for its power reading instead of "not reported"
- Memory module speed is labelled MT/s, the unit SMBIOS reports, not MHz; committed memory above the commit limit is no longer flagged, since the limit is enforced only with `vm.overcommit_memory=2`
- CPU frequencies round to whole MHz, and the run queue (`procs_running`) is labelled runnable rather than running processes
- **`talos_net_link_up` is 1 for live WireGuard links**: they report operational state `unknown` forever, so every tunnel read as down on the metric and the Network page. The `UP` and `LOWER_UP` flags now decide for such links, as `ip link` does
- The CPU page's usage card read "0 cores / 0 threads": the page never loaded the totals it shows
- An NVIDIA card's GPU page no longer advises granting `os:admin`, which cannot expose its counters
- `.dockerignore` listed `git` instead of `.git`, so every image build uploaded the repository history as build context
- **CPU frequency read 0 Hz under `os:reader`**: `talos_node_cpu_freq_*` are absent when nothing measured them, instead of a confident 0; the CPU page named a metric that did not exist
- **GPU page said "No GPU with a driver bound"** while the GPU was present and only its counters were refused; GPUs without `os:admin` are now named with model and PCI address
- **Rotated Talos clients failed in-flight collections** on every 6 h cert renewal; replaced clients now close after a 30 s grace period
- A collector that stops applying to a node (a demoted control plane) no longer re-serves its last data forever
- The `collector_degraded` log hint named `os:admin` for every reason, including a stale inventory
- The version chip printed a doubled `v`

### Removed
- The Forgejo workflows (`.forgejo/`): a hand-kept copy of the GitHub ones that had already drifted to a Go version `go.mod` rejects. GitHub Actions is the only CI

## [0.1.1] - 2026-10-09

### Fixed
- `/ready` requires a loaded talosconfig, so the pod is not Ready while every collector fails
- The talosconfig Secret is mounted optionally: a cluster without `kubernetesTalosAPIAccess` gets the exporter's own "talosconfig unavailable" instead of a stuck `ContainerCreating`
- Four dashboard arithmetic bugs
- A failed Gather was cached in the exposition cache
- Idle keep-alive connections had no deadline
- Collectors that serve partial data are reported (`talos_monitoring_collector_degraded`)
- govulncheck findings CI scans for

### Changed
- Go 1.27.2 and a golangci-lint built with it

## [0.1.0] - 2026-09-26

### Added
- Initial public release: an agentless Prometheus exporter and htmx dashboard for Talos node hardware, with a Helm chart

[Unreleased]: https://github.com/yuriy-kovalchuk/talos-monitoring/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/yuriy-kovalchuk/talos-monitoring/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/yuriy-kovalchuk/talos-monitoring/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/yuriy-kovalchuk/talos-monitoring/releases/tag/v0.1.0
