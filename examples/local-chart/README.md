# Deploy the local chart with the latest remote image

Use this to try chart changes (templates, values, new resources) from a
checkout against the newest green main build the pipeline published — no local
docker build, no release tag to create.

The chart in this repo pins the image tag to its `appVersion`; the override
below retargets it at `:main`, the mutable tag CI updates on every green push
to main (release tag pushes still drive `:latest` via CD).

From the **repo root**:

```sh
helm install talos-monitoring ./charts/talos-monitoring \
  -f examples/local-chart/values.yaml \
  --namespace talos-monitoring --create-namespace
```

Iterating on chart changes:

```sh
helm upgrade talos-monitoring ./charts/talos-monitoring \
  -f examples/local-chart/values.yaml --namespace talos-monitoring

# or render without touching the cluster:
helm template talos-monitoring ./charts/talos-monitoring \
  -f examples/local-chart/values.yaml --kube-version v1.36.1
```

Caveats:

- `:main` is mutable, so the example values set `pullPolicy: Always` —
  otherwise a node with a stale cached image keeps running it. Never use this
  pattern for a stable install; use [../remote/](../remote/) instead.
- The local chart's `version`/`appVersion` are whatever the checkout has;
  only the image tag is overridden.
