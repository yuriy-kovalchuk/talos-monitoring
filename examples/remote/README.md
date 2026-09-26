# Deploy from the remote (released) chart

The CD pipeline publishes a chart to GHCR for every `vX.Y.Z` tag push.
The chart's image defaults to the matching
`ghcr.io/yuriy-kovalchuk/talos-monitoring:vX.Y.Z`, so a released
install needs no image override.

> Requires a release to exist — the registry is empty until the first tag is
> pushed. Until then, use [../local-chart/](../local-chart/).

## 1. Pick a version

Charts have no `:latest` (chart versions must be unique semver). Published
versions are listed under the repo's GitHub Packages page (package
`talos-monitoring`).

## 2. Install

```sh
helm install talos-monitoring oci://ghcr.io/yuriy-kovalchuk/charts/talos-monitoring \
  --version <version> \
  -f values.yaml \
  --namespace talos-monitoring --create-namespace
```

(add `helm registry login ghcr.io -u <github-username>` first if the package
is still private)

`values.yaml` here is an example override set (ServiceMonitor, Grafana
dashboard). Omit `-f values.yaml` to run with pure chart defaults.

## Upgrade / rollback

```sh
helm upgrade talos-monitoring oci://ghcr.io/yuriy-kovalchuk/charts/talos-monitoring \
  --version <new-version> -f values.yaml --namespace talos-monitoring

helm rollback talos-monitoring --namespace talos-monitoring
```
