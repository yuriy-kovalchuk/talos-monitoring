# Deployment examples

Two ways to run talos-monitoring, both in-cluster (the app authenticates to the
Talos API with its own ServiceAccount — no `talosctl`, nothing runs on the node).

| Example | Chart source | Image | Use when |
|---|---|---|---|
| [`remote/`](remote/) | released chart from GHCR (`oci://…`) | the chart's pinned release version | stable installs of a released version |
| [`local-chart/`](local-chart/) | chart from this checkout (`charts/talos-monitoring`) | `:main`, the newest green main build | testing chart changes against the newest image, without building anything locally |

## Common prerequisites

1. **Every Talos node** must have the in-cluster API-access feature enabled with
   the release namespace listed in `allowedKubernetesNamespaces` — see
   [Prerequisites in the main README](../README.md#prerequisites). Until it is,
   the pod stays 0/1 Ready and logs `talosconfig unavailable, retrying` — that is
   expected, not a crash.

2. **Helm login** to the registry (needed for chart pulls, and for image pulls
   while the GHCR package is private):

   ```sh
   helm registry login ghcr.io -u <github-username>
   ```

3. **Verify** after install:

   ```console
   $ kubectl -n talos-monitoring get pods        # 1/1 Ready
   $ kubectl -n talos-monitoring port-forward svc/talos-monitoring 8080:8080
   $ curl -s localhost:8080/metrics | head       # metrics; / in a browser for the dashboard
   ```
