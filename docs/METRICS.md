# Metrics reference

Every metric `talos-monitoring` exports — **180 families**, 4 311 series on the reference
cluster (4 nodes, 40 threads, 105 PCI devices, 47 disks, 45 block devices, 40 sensors,
46 services, 18 extension entries, 124 kernel params, 72 modules, 88 volumes, 3 GPUs,
`--collectors.cpu.mode-seconds` on).

Examples below are real values from that cluster. `node` is on every per-node metric, so
it's left out of the tables. **†** marks a counter (everything else is a gauge). **‡**
marks an example that's illustrative only — the family is conditional and was absent on
the reference cluster.

## Conventions

- **Base units in the value slot** — `_bytes`, `_seconds`, `_hertz`, `_celsius`, `_volts`,
  `_watts`, `_transfers_per_second`, ratios 0–1. Never `_mhz`, `_percent`, `_gtps`.
- **One family, one unit.** A label never carries the unit — that's why sensors are five
  families instead of one keyed by `kind`.
- **`_info` metrics are value-1 gauges** holding identity strings only. Every measurement
  gets its own gauge, keyed by the same identity.
- **Identifiers are labels, never values** (`irq`, `numa_node`, `governor`). An unknown
  value means the series is absent — never a `-1` sentinel.
- **A measurement and its limits are separate families** — e.g. `_freq_hertz` vs
  `_freq_min_hertz` — so `avg()` never mixes a live reading with a policy ceiling.
- **Mutable attributes get their own `_info` metric.** A change then moves a series
  instead of ending one and starting another.
- **An unknown value is always an absent series — even a hardware sentinel.** NVMe drives
  report an unset threshold at both ends: `-273150` m°C (0 Kelvin) as the min,
  `65261850` m°C as the max. Exporting them made `min()` read absolute zero and `max()`
  read 65261 °C — and any percent-of-limit calculation on that sensor came out near zero.
- **Free text gets its own `_info` metric, present only when there is text** — a volume
  error, a diagnostic message, an unmet condition's reason. Putting it on the status
  metric instead would move that series every time the wording changed, and add an
  empty label to every healthy series.
- Counters carry `_total` and start at zero, so `rate()` works before the first event.
- **Two of our label names collide with Kubernetes' own scrape labels**: `service` (on
  `talos_service_*`) and `namespace` (on `talos_pv_info`). Prometheus attaches both of
  those to every scraped target too. By default (`honor_labels: false`) it keeps its own
  and silently renames ours to `exported_service` / `exported_namespace` — which then
  makes `sum by (service)` merge twelve different Talos services into one row, with no
  error. The chart's ServiceMonitor sets `honorLabels: true`
  (`serviceMonitor.honorLabels`) to prevent this. Scraping another way? Set the same
  option, or rename the labels yourself in `metric_relabel_configs`.

### CPU

| Metric | Labels | Example |
|---|---|---|
| `talos_node_cpu_freq_hertz` | `cpu` | `cpu=0` → **2.31605e+09** |
| `talos_node_cpu_freq_max_hertz` | `cpu` | `cpu=0` → **4.78708e+09** |
| `talos_node_cpu_freq_min_hertz` | `cpu` | `cpu=0` → **1.09584e+09** |
| `talos_node_cpu_governor_info` | `cpu`, `governor` | `cpu=0, governor=powersave` → **1** |
| `talos_node_cpu_seconds_total` † | `cpu`, `mode` | `cpu=0, mode=idle` → **1.06001e+06** |
| `talos_node_cpu_usage_ratio` | `cpu` | `cpu=0` → **0.0694915** |
| `talos_node_cpu_mode_ratio` | `mode` | `mode=user` → **0.0667** (the ten modes sum to 1) |

### Memory & swap

| Metric | Labels | Example |
|---|---|---|
| `talos_node_memory_anon_bytes` | — | **2.20818e+10** |
| `talos_node_memory_available_bytes` | — | **8.72005e+09** |
| `talos_node_memory_buffers_bytes` | — | **1.27275e+08** |
| `talos_node_memory_cached_bytes` | — | **7.8212e+09** |
| `talos_node_memory_commit_limit_bytes` | — | **1.6435e+10** |
| `talos_node_memory_committed_bytes` | — | **4.90963e+10** |
| `talos_node_memory_dirty_bytes` | — | **9519104** |
| `talos_node_memory_free_bytes` | — | **9.68077e+08** |
| `talos_node_memory_hugepage_size_bytes` | — | **2097152** |
| `talos_node_memory_hugepages` | `state` | `state=free` → **0** |
| `talos_node_memory_kernel_stack_bytes` | — | **5.28343e+07** |
| `talos_node_memory_mapped_bytes` | — | **2.95356e+09** |
| `talos_node_memory_page_tables_bytes` | — | **1.15622e+08** |
| `talos_node_memory_shared_bytes` | — | **2.95842e+08** |
| `talos_node_memory_slab_bytes` | — | **1.08412e+09** |
| `talos_node_memory_slab_reclaimable_bytes` | — | **5.75046e+08** |
| `talos_node_memory_total_bytes` | — | **3.28701e+10** |
| `talos_node_memory_writeback_bytes` | — | **0** |
| `talos_node_swap_cached_bytes` | — | **0** |
| `talos_node_swap_free_bytes` | — | **0** |
| `talos_node_swap_total_bytes` | — | **0** |

### Node identity & liveness

`talos_node_k8s_info` is the one family that doesn't come from the Talos API — discovery
reads it from the Kubernetes node object instead. It's exported because the dashboard
shows those columns, and everything the dashboard shows must also be at `/metrics`.

| Metric | Labels | Example |
|---|---|---|
| `talos_node_processes` | `state` | `state=running` → **4** |
| `talos_node_k8s_info` | `ip`, `hostname`, `roles`, `kube_version`, `os`, `arch` | `ip=192.0.2.3, hostname=controlplane-1, roles=control-plane, kube_version=v1.36.3, os=linux, arch=amd64` → **1** |
| `talos_node_up` | — | **1** |
| `talos_node_uptime_seconds` | — | **1.23436e+06** |
| `talos_node_version_info` | `arch`, `built`, `sha`, `version` | `arch=amd64, built=, sha=707dbd89, version=v1.13.4` → **1** |

`talos_node_up` is the single source of truth for reachability — a verdict over a window
of time, not just the last round. **While `talos_node_up` is 0, that node's other metrics
are absent from the exposition.** The exporter drops a down node's cached data so
Prometheus marks the series stale, instead of quietly serving the last good value forever.
The liveness signal itself and `talos_monitoring_node_scrape_errors_total` keep exporting
either way, and the node's data comes back on its first successful collection.

### Health & services

Three service families instead of one state gauge, because Talos itself reports three
independent booleans. `health_unknown=1` means *nothing has ever checked* — not that a
check failed. Talos sets it whenever `status.Healthy == nil`, which happens for two kinds
of service by construction: health checks are opt-in (`dashboard` doesn't implement one),
and extension services have no health-check field at all, so every `ext-*` service is
always unknown.

Those two kinds of service report `healthy=0` and `health_unknown=1` forever, so
**`talos_service_healthy == 0` alone is not an alert** — on every reference node it fires
permanently for 2 of 12 services. The condition that actually means something is broken:

```promql
talos_service_healthy == 0 and talos_service_health_unknown == 0
```

`talos_diagnostics` and `talos_machine_unmet_conditions` start at zero because their
companion `_info` families have no series at all on a healthy node. Without a zero to
show, "healthy" and "the collector never ran" would look the same.

| Metric | Labels | Example |
|---|---|---|
| `talos_service_running` | `service` | `service=etcd` → **1** |
| `talos_service_healthy` | `service` | `service=etcd` → **1** |
| `talos_service_health_unknown` | `service` | `service=dashboard` → **1** |
| `talos_machine_stage_info` | `stage` | `stage=running` → **1** |
| `talos_machine_ready` | — | **1** |
| `talos_machine_unmet_conditions` | — | **0** |
| `talos_machine_unmet_condition_info` | `condition`, `reason` | `condition=etcd, reason=etcd is not healthy` → **1** (absent when ready) |
| `talos_diagnostics` | — | **0** |
| `talos_diagnostic_info` | `id`, `message` | `id=address-overlap` → **1** (absent when none) |

### Image & security

What software the node is running. `Versions.runtime` is deliberately **not** a source
here — it only carries `Talos <version>`, which `talos_node_version_info` already
exports from the machine API.

The schematic gets its own family because it answers a fleet-consistency question in one
query — how many distinct images does the fleet run:

```promql
count(count by (schematic) (talos_schematic_info))
```

On the reference cluster that's 2, correctly: `controlplane-1` and `worker-1` have AMD
GPUs and run a schematic with `amd-ucode` + `amdgpu`; the other two don't. The same
pattern over `talos_kernel_cmdline_info` and `talos_booted_entry_info` catches a kernel
argument that never took effect, or a node still booted from the other A/B slot.

`schematic` and `modules.dep` are virtual entries Talos reports alongside real
extensions. They show up in `talos_extension_info` because that's what the node reports,
but they aren't installed software — so the UI counts 4 extensions where the metric has
6 series.

| Metric | Labels | Example |
|---|---|---|
| `talos_extension_info` | `extension`, `version` | `extension=amdgpu, version=20260519-v1.13.4` → **1** |
| `talos_schematic_info` | `schematic` | `schematic=65cf8364cd0d…` → **1** |
| `talos_kernel_cmdline_info` | `cmdline` | `cmdline=talos.platform=metal console=tty0 …` → **1** |
| `talos_booted_entry_info` | `entry` | `entry=talos-v1.13.4~1.efi` → **1** |
| `talos_security_secure_boot` | — | **0** |
| `talos_security_booted_with_uki` | — | **1** |
| `talos_security_module_signature_enforced` | — | **1** |
| `talos_security_selinux_state_info` | `state` | `state=enabled, permissive` → **1** |
| `talos_security_fips_state_info` | `state` | `state=disabled` → **1** |

### Clock

`talos_time_synced` and `talos_time_kernel_synced` are **not** the same fact. The first is
Talos's own ntpd verdict on its time servers; the second is the kernel's `STA_UNSYNC`
flag. Both exist because they disagree exactly when something interesting is happening.
Clock skew silently breaks etcd, TLS and storage auth, so `talos_time_synced == 0` for
5 minutes is a genuine page.

`talos_time_epoch` counts clock steps since boot. It's a gauge, not a `_total`, because it
resets on reboot — a counter would turn that reset into a spurious negative rate. Alert on
`changes(talos_time_epoch[1h]) > 0`: a step invalidates any duration measured across it.

| Metric | Labels | Example |
|---|---|---|
| `talos_time_synced` | — | **1** |
| `talos_time_kernel_synced` | — | **1** |
| `talos_time_sync_disabled` | — | **0** |
| `talos_time_epoch` | — | **0** |
| `talos_time_offset_seconds` | — | **-4.4597e-05** |
| `talos_time_max_error_seconds` | — | **0.46** |
| `talos_time_est_error_seconds` | — | **0** |
| `talos_time_frequency_adjustment_ratio` | — | **1.0000331520996093** |
| `talos_time_state_info` | `state` | `state=TIME_OK` → **1** |

### Kernel tunables & modules

Parameter values are **labels**, not gauge values, deliberately: a tunable has no unit and
nothing to average — its value might be a number, a list (`0 0 0`), a mode word, or a
path. The question you actually ask is "which nodes differ", and that's a label
comparison. Only two genuine booleans get real gauges. The important one is
`talos_kernel_param_unsupported`: the running kernel doesn't recognise the parameter, so
a value set in the machine configuration is **silently ignored** — nothing else anywhere
reports that. `talos_kernel_param_drifted` is 1 wherever `current` and `default` disagree.
It's a gauge because PromQL can't compare two labels on the same series, so "how many
tunables differ from default" would otherwise be unanswerable from the export.

**`default` isn't always the kernel's compiled-in default.** Talos records the value it
read *before* writing its own, and restores that value if the setting is removed. For most
tunables that's the compile-time default; `proc.sys.user.max_user_namespaces` is an
exception — it's 247716 on the reference cluster, a number the kernel sizes from the
node's memory at boot.

Module `dependencies` aren't exported — the module's presence is the fact worth alerting
on, and the dependency graph belongs in the UI table instead.

**This is the most expensive collector in the project:** 149 series per node on the
reference cluster (31 params × 3, 18 modules × 3). It's Inventory-class with a 1h TTL, so
collecting it costs almost nothing — but if the series budget matters,
`--collectors.kernel.enabled=false` is the lever.

| Metric | Labels | Example |
|---|---|---|
| `talos_kernel_param_info` | `param`, `current`, `default` | `param=proc.sys.fs.aio-max-nr, current=1048576, default=65536` → **1** |
| `talos_kernel_param_unsupported` | `param` | **0** |
| `talos_kernel_params_unsupported` | — | **0** |
| `talos_kernel_param_drifted` | `param` | `param=proc.sys.fs.aio-max-nr` → **1** |
| `talos_kernel_params_drifted` | — | **22** |
| `talos_kernel_module_info` | `module`, `state` | `module=amdgpu, state=Live` → **1** |
| `talos_kernel_module_size_bytes` | `module` | **1.6965632e+07** |
| `talos_kernel_module_reference_count` | `module` | **2** |
| `talos_kernel_modules` | — | **19** |

### Talos volume layer

**`talos_machine_volume_*`, not `talos_volume_*`.** That name was already taken by the
block collector, for Kubernetes PersistentVolumes keyed by a `pv` label — a completely
different thing. These families are Talos's own volumes: `EPHEMERAL`, `STATE`, `META`, and
the directory/overlay/symlink pseudo-volumes Talos also models here.

Talos reports 22 volumes per reference node, and only 3 are real partitions. Every series
carries `type` so a query can filter to `type="partition"` — instead of the exporter
silently dropping the rest, since even a *failed* pseudo-volume is worth seeing.

`talos_mount_*` deliberately carries no usage bytes: `MountStatus` doesn't have them, and
`talos_filesystem_*` (from the Mounts RPC) already does. What's unique here is the
metadata — read-only, project quota support, encryption provider.

| Metric | Labels | Example |
|---|---|---|
| `talos_machine_volume_info` | `volume`, `type`, `phase`, `filesystem`, `location` | `volume=EPHEMERAL, type=partition, phase=ready, filesystem=xfs, location=/dev/nvme0n1p4` → **1** |
| `talos_machine_volume_ready` | `volume`, `type` | **1** |
| `talos_machine_volume_capacity_bytes` | `volume`, `type` | **1.25725310976e+11** (absent for directories) |
| `talos_machine_volume_encrypted` | `volume`, `provider` | `provider=luks2` → **1** (absent when unencrypted) |
| `talos_machine_volume_encryption_failed_syncs` | `volume` | **0** |
| `talos_machine_volume_error_info` | `volume`, `error` | `volume=EPHEMERAL, error=failed to unlock: no key slot matched` → **1** (absent on a healthy node) |
| `talos_machine_volumes_not_ready` | — | **0** |
| `talos_mount_info` | `volume`, `source`, `target`, `filesystem` | `volume=EPHEMERAL, source=/dev/nvme0n1p4, target=/var, filesystem=xfs` → **1** |
| `talos_mount_read_only` | `volume` | **0** |
| `talos_mount_project_quota_supported` | `volume` | **1** |
| `talos_system_disk_info` | `disk`, `dev_path` | `disk=nvme0n1, dev_path=/dev/nvme0n1` → **1** |

### GPU

Utilisation and VRAM, read from `/sys/class/drm/cardN/device/`. **Temperature, fan, power
cap and clocks are not here** — the sensors collector already picks up amdgpu, since it
matches hwmon chips generically, so exporting them again here would just be a second
source for the same quantity.

This is file-based, so it needs `os:admin` and self-disables on a permission error, the
same as `sensors`. A driver that doesn't publish a counter produces an **absent series,
never a zero**: an Intel iGPU reports no utilisation at all, and showing 0% busy would be
a lie. A node with no GPU driver bound has no `/sys/class/drm` tree and reports only
`talos_gpus 0`.

There's one exception to absent-never-zero: a card whose PCI device is
**runtime-suspended** (asleep — amdgpu suspends idle discrete cards after ~10s) can't
answer sysfs reads at all. The collector reports its utilisation as a **derived 0** (an
asleep GPU does no work) and exports the sleep state itself as
`talos_gpu_runtime_suspended`, so that zero stays distinguishable from an awake, genuinely
idle card. Memory and GTT stay absent until the card wakes, and the UI shows the state
(`asleep`) instead of a value. A driver that truly doesn't publish a file at all — i915 has
no `gpu_busy_percent` — still produces an absent series, asleep or awake.

`pci` holds the BDF, which joins onto `talos_hw_pcidevice_info` for vendor and model.

| Metric | Labels | Example |
|---|---|---|
| `talos_gpu_info` | `card`, `driver`, `pci_id`, `pci` | `card=card0, driver=amdgpu, pci_id=1002:7551, pci=0000:03:00.0` → **1** |
| `talos_gpu_utilization_ratio` | `card` | **0** (a ratio 0–1, not the percent sysfs reports; a derived 0 while the card is runtime-suspended) |
| `talos_gpu_runtime_suspended` | `card` | **1** (0 while the device is awake; absent where the driver publishes no `power/runtime_status`) |
| `talos_gpu_memory_total_bytes` | `card` | **3.4208743424e+10** |
| `talos_gpu_memory_used_bytes` | `card` | **3.2394584064e+10** |
| `talos_gpu_gtt_total_bytes` | `card` | **3.2529408e+07** |
| `talos_gpu_gtt_used_bytes` | `card` | **4.61893632e+08** |
| `talos_gpus` | — | **2** |

### Block I/O

**`talos_block_io_*`, not `talos_disk_*`.** The `block` collector already owns
`talos_block_disk_*` for disk *inventory* (model, serial, WWID, size), keyed by the same
`device` label. Two prefixes for one object class — each with its own `_info` family —
is the same trap `talos_volume_*` sprang earlier, so sharing the `talos_block_*` subsystem
keeps the relationship obvious instead.

Per-device counters, read from the `DiskStats` RPC — the node's `/proc/diskstats`. This is
the half of storage the `block` collector doesn't cover: it knows a disk's model, WWID,
capacity and fullness, but none of that says whether the disk is doing any work.
`os:reader`-safe, so unlike the sysfs collectors this needs no `os:admin` and has no
self-disable path.

**Sectors are always 512 bytes.** The kernel reports `/proc/diskstats` in 512-byte units
no matter the device's real logical or physical sector size — multiplying by an NVMe's own
4096-byte sectors would overstate throughput eightfold. The conversion is pinned by
`TestSectorsAreAlwaysFiveTwelveBytes`.

**Partitions get counted twice.** A partition's I/O is also counted in its parent disk, so
summing across every device double-counts every partitioned disk. `talos_block_io_info`
carries a `kind` label (`disk` / `partition` / `virtual`), worked out by matching each
device name against the others in the same response — join on it to pick one level:

```promql
sum(rate(talos_block_io_read_bytes_total[5m])) by (node)
  * on(node,device) group_left talos_block_io_info{kind="disk"}
```

Loop, ram and fd devices are dropped — Talos mounts every system extension as a loop
device (8 per reference node) and there's no real hardware behind any of them. `zram` is
kept and labelled `virtual`, since compressed swap is real traffic. The RPC's pre-summed
`Total` row is ignored, for the same double-counting reason as above.

At 16 series per device this is the second most expensive collector after `kernel`
(180 per reference node, inflated there by iSCSI PersistentVolumes).
`--collectors.diskio.enabled=false` is the lever.

| Metric | Labels | Example |
|---|---|---|
| `talos_block_io_info` | `device`, `kind` | `device=nvme0n1, kind=disk` → **1** |
| `talos_block_io_reads_completed_total` † | `device` | **1.23456e+06** |
| `talos_block_io_reads_merged_total` † | `device` | **8123** |
| `talos_block_io_read_bytes_total` † | `device` | **4.987473539072e+12** |
| `talos_block_io_read_time_seconds_total` † | `device` | **312.4** |
| `talos_block_io_writes_completed_total` † | `device` | **9.87654e+05** |
| `talos_block_io_writes_merged_total` † | `device` | **4210** |
| `talos_block_io_written_bytes_total` † | `device` | **9.7526271744e+11** |
| `talos_block_io_write_time_seconds_total` † | `device` | **1804.9** |
| `talos_block_io_now` | `device` | **0** (in flight now — a gauge, not a counter) |
| `talos_block_io_time_seconds_total` † | `device` | **111610.7** (`rate()` = utilisation 0–1) |
| `talos_block_io_time_weighted_seconds_total` † | `device` | **48221.3** (`rate()` = mean queue length) |
| `talos_block_io_discards_completed_total` † | `device` | **0** |
| `talos_block_io_discards_merged_total` † | `device` | **0** |
| `talos_block_io_discarded_bytes_total` † | `device` | **0** |
| `talos_block_io_discard_time_seconds_total` † | `device` | **0** |

### Load average

Three series per node, from the `LoadAvg` RPC. Best-effort inside the `nodeapi`
collector: if that one RPC fails, these series are **absent**, not zero — a 0.0 load
would read as an idle machine. Version, uptime and process counts still get collected.

| Metric | Labels | Example |
|---|---|---|
| `talos_node_load1` | — | **1.92** |
| `talos_node_load5` | — | **1.53** |
| `talos_node_load15` | — | **1.47** |

### System & processor

| Metric | Labels | Example |
|---|---|---|
| `talos_hw_processor_boot_hertz` | `socket` | `socket=FP7r2` → **3.2e+09** |
| `talos_hw_processor_cores` | `socket` | `socket=FP7r2` → **8** |
| `talos_hw_processor_cores_enabled` | `socket` | `socket=FP7r2` → **8** |
| `talos_hw_processor_enabled` | `socket` | `socket=FP7r2` → **1** |
| `talos_hw_processor_info` | `asset_tag`, `part_number`, `product`, `serial_number`, `socket` | `asset_tag=Unknown, part_number=Unknown, product=AMD Ryzen 7 6800H with Radeon …, serial_number=Unknown, socket=FP7r2` → **1** |
| `talos_hw_processor_max_hertz` | `socket` | `socket=FP7r2` → **4.75e+09** |
| `talos_hw_processor_socket_populated` | `socket` | `socket=FP7r2` → **1** |
| `talos_hw_processor_status` | `socket` | `socket=FP7r2` → **65** |
| `talos_hw_processor_threads` | `socket` | `socket=FP7r2` → **16** |
| `talos_hw_system_info` | `manufacturer`, `product`, `serial_number`, `sku_number`, `uuid`, `version`, `wake_up_type` | `manufacturer=Micro Computer (HK) Tech Limit…, product=AI Series, serial_number=MD507LX125QQMQE00042, sku_number=MGF8BSC, uuid=ad362a00-9b37-11f0-9f3d-0c6489…, version=1.0, wake_up_type=Power Switch` → **1** |

### Memory modules

| Metric | Labels | Example |
|---|---|---|
| `talos_hw_memory_module_info` | `asset_tag`, `bank_locator`, `device_locator`, `manufacturer`, `product`, `serial_number`, `slot` | `asset_tag=, bank_locator=ChannelA, device_locator=DIMM3, manufacturer=Unknown - [0x0000], product=BPCMEMNB83200, serial_number=0000001A, slot=DIMM3-ChannelA` → **1** |
| `talos_hw_memory_module_size_bytes` | `slot` | `slot=DIMM-0-P0-CHANNEL-A` → **8.58993e+09** |
| `talos_hw_memory_module_speed_transfers_per_second` | `slot` | `slot=DIMM-0-P0-CHANNEL-A` → **6e+09** |

### PCI

| Metric | Labels | Example |
|---|---|---|
| `talos_hw_pcidevice_aer_errors_total` † | `bdf`, `severity` | `bdf=0000:00:01.1, severity=correctable` → **0** |
| `talos_hw_pcidevice_enabled` | `bdf` | `bdf=0000:00:00.2` → **1** |
| `talos_hw_pcidevice_info` | `bdf`, `class`, `class_id`, `driver`, `product`, `product_id`, `revision`, `subclass`, `subclass_id`, `subsystem_product_id`, `subsystem_vendor_id`, `vendor`, `vendor_id` | `bdf=0000:00:01.1, class=Bridge, class_id=0x06, driver=pcieport, product=Family 17h-19h PCIe GPP Bridge, product_id=0x14b8, revision=0x00, subclass=PCI bridge, subclass_id=0x04, subsystem_product_id=0x1453, subsystem_vendor_id=0x1022, vendor=Advanced Micro Devices, Inc. […, vendor_id=0x1022` → **1** |
| `talos_hw_pcidevice_irq_info` | `bdf`, `irq` | `bdf=0000:00:00.0, irq=0` → **1** |
| `talos_hw_pcidevice_link_max_speed_transfers_per_second` | `bdf` | `bdf=0000:00:01.1` → **1.6e+10** |
| `talos_hw_pcidevice_link_max_width_lanes` | `bdf` | `bdf=0000:00:01.1` → **8** |
| `talos_hw_pcidevice_link_speed_transfers_per_second` | `bdf` | `bdf=0000:00:01.1` → **8e+09** |
| `talos_hw_pcidevice_link_width_lanes` | `bdf` | `bdf=0000:00:01.1` → **4** |
| `talos_hw_pcidevice_power_state_info` | `bdf`, `state` | `bdf=0000:00:00.0, state=D0` → **1** |
| `talos_hw_pcidevice_numa_node_info` | `bdf`, `numa_node` | `bdf=0000:03:00.0, numa_node=0` → **1** (absent on devices that report none) |

### Sensors

**A sensor's value file isn't always `*_input`.** A discrete AMD card publishes only
`power1_average` — matching `_input` alone picked up the integrated GPU's 7W and missed
the 300W card sitting beside it. Where a chip publishes both, `_input` wins and
`_average` isn't exported as a second sensor. Board power limits work the same way:
amdgpu puts them in `power1_cap` and `power1_cap_max`, so `limit` carries **`cap`** and
**`cap max`**, alongside `critical`, `max` and `min`.

`talos_sensor_chip_info` is what attributes a reading to a device. Everything under
`/sys/class/hwmon` is a symlink into `/sys/devices`, and the **last** PCI address in that
path is the device that actually owns the chip — the ones before it are just bridges.
Without this, a two-GPU node's temperatures can't be told apart, and guessing from hwmon
numbering mislabels them. Join on `chip`:

```promql
talos_sensor_temperature_celsius
  * on (node, chip) group_left (pci) talos_sensor_chip_info
```


| Metric | Labels | Example |
|---|---|---|
| `talos_sensor_chip_info` | `chip`, `chip_name`, `pci` | `chip=/sys/class/hwmon/hwmon3, chip_name=amdgpu, pci=0000:03:00.0` → **1** (absent when the chip has no PCI parent) |
| `talos_sensor_alarm` | `chip`, `chip_name`, `kind`, `label`, `sensor` | `chip=/sys/class/hwmon/hwmon2, chip_name=nvme, kind=temperature, label=Composite, sensor=temp1` → **0** |
| `talos_sensor_fan_limit_rpm` | `chip`, `chip_name`, `label`, `limit`, `sensor` | `chip=/sys/class/hwmon/hwmon3, chip_name=amdgpu, label=, limit=max, sensor=fan1` → **5100** |
| `talos_sensor_fan_rpm` | `chip`, `chip_name`, `label`, `sensor` | `chip=/sys/class/hwmon/hwmon3, chip_name=amdgpu, label=, sensor=fan1` → **1199** |
| `talos_sensor_frequency_hertz` | `chip`, `chip_name`, `label`, `sensor` | `chip=/sys/class/hwmon/hwmon3, chip_name=amdgpu, label=mclk, sensor=freq2` → **9.6e+07** |
| `talos_sensor_frequency_limit_hertz` | `chip`, `chip_name`, `label`, `limit`, `sensor` | `chip=/sys/class/hwmon/hwmon3, chip_name=amdgpu, label=mclk, limit=max, sensor=freq2` → **1.2e+08** ‡ |
| `talos_sensor_power_limit_watts` | `chip`, `chip_name`, `label`, `limit`, `sensor` | `chip=/sys/class/hwmon/hwmon3, chip_name=amdgpu, label=PPT, limit=cap, sensor=power1` → **300** |
| `talos_sensor_power_watts` | `chip`, `chip_name`, `label`, `sensor` | `chip=/sys/class/hwmon/hwmon4, chip_name=amdgpu, label=PPT, sensor=power1` → **7.042** |
| `talos_sensor_temperature_celsius` | `chip`, `chip_name`, `label`, `sensor` | `chip=/sys/class/hwmon/hwmon1, chip_name=k10temp, label=Tctl, sensor=temp1` → **45.625** |
| `talos_sensor_temperature_limit_celsius` | `chip`, `chip_name`, `label`, `limit`, `sensor` | `chip=/sys/class/hwmon/hwmon2, chip_name=nvme, label=Composite, limit=critical, sensor=temp1` → **84.85** |
| `talos_sensor_voltage_limit_volts` | `chip`, `chip_name`, `label`, `limit`, `sensor` | `chip=/sys/class/hwmon/hwmon3, chip_name=amdgpu, label=vddgfx, limit=critical, sensor=in0` → **1.0** ‡ |
| `talos_sensor_voltage_volts` | `chip`, `chip_name`, `label`, `sensor` | `chip=/sys/class/hwmon/hwmon3, chip_name=amdgpu, label=vddgfx, sensor=in0` → **0.029** |

The `*_limit_*` families are **conditionally exported**: a limit gauge exists only where
the driver publishes a threshold (`limit` = `critical`, `max`, or `min`). The reference
cluster only published temperature and fan limits, so the `frequency`, `power` and
`voltage` limit examples above are illustrative (‡), not real readings from that cluster.

### Network

| Metric | Labels | Example |
|---|---|---|
| `talos_net_address_info` | `address`, `family`, `link`, `scope` | `address=192.0.2.3/24, family=inet4, link=eno1, scope=global` → **1** |
| `talos_net_link_carrier` | `link` | `link=eno1` → **1** |
| `talos_net_link_info` | `bus_path`, `driver`, `driver_version`, `duplex`, `firmware_version`, `hwaddr`, `kind`, `link`, `master`, `pci_id`, `port`, `product`, `type`, `vendor` | `bus_path=0000:00:1f.6, driver=e1000e, driver_version=6.18.34-talos, duplex=Full, firmware_version=0.8-4, hwaddr=fc:3f:db:0f:8e:18, kind=, link=eno1, master=, pci_id=8086:15B7, port=TwistedPair, product=Ethernet Connection (2) I219-L…, type=ether, vendor=Intel Corporation` → **1** |
| `talos_net_link_mtu_bytes` | `link` | `link=bond0` → **1500** |
| `talos_net_link_rx_bytes_per_second` | `link` | `link=eno1` → **7.68669e+06** |
| `talos_net_link_rx_bytes_total` † | `link` | `link=eno1` → **7.90789e+12** |
| `talos_net_link_rx_dropped_total` † | `link` | `link=eno1` → **53** |
| `talos_net_link_rx_errors_total` † | `link` | `link=bond0` → **0** |
| `talos_net_link_rx_packets_total` † | `link` | `link=eno1` → **6.72807e+09** |
| `talos_net_link_speed_bits_per_second` | `link` | `link=eno1` → **2.5e+09** |
| `talos_net_link_tx_bytes_per_second` | `link` | `link=eno1` → **795684** |
| `talos_net_link_tx_bytes_total` † | `link` | `link=eno1` → **9.36109e+12** |
| `talos_net_link_tx_dropped_total` † | `link` | `link=eno1` → **445** |
| `talos_net_link_tx_errors_total` † | `link` | `link=bond0` → **0** |
| `talos_net_link_tx_packets_total` † | `link` | `link=eno1` → **7.25061e+09** |
| `talos_net_link_up` | `link` | `link=eno1` → **1** |

`master` on `talos_net_link_info` names the bond or bridge an interface is enslaved to, and
is empty for a topmost interface. A bond's byte counters already include its slaves', so a
per-node throughput sum has to filter `master=""`; adding every interface reports a bonded
node at roughly twice its traffic. On the reference cluster `bond0` exists but has no
slaves, so every link there has an empty `master` — the double count is latent, not
something that cluster shows.

### Disks

| Metric | Labels | Example |
|---|---|---|
| `talos_block_disk_cdrom` | `device` | `device=/dev/nvme0n1` → **0** |
| `talos_block_disk_info` | `attachment`, `bus_path`, `device`, `model`, `serial`, `subsystem`, `transport`, `uuid`, `wwid` | `attachment=local, bus_path=/pci0000:00/0000:00:02.4/0000:…, device=/dev/nvme0n1, model=CT1000P310SSD8, serial=2535527376C6, subsystem=/sys/class/block, transport=nvme, uuid=, wwid=eui.00a07501527376c6` → **1** |
| `talos_block_disk_readonly` | `device` | `device=/dev/nvme0n1` → **0** |
| `talos_block_disk_rotational` | `device` | `device=/dev/sdd` → **1** |
| `talos_block_disk_sector_size_bytes` | `device` | `device=/dev/nvme0n1` → **512** |
| `talos_block_disk_size_bytes` | `device` | `device=/dev/nvme0n1` → **1.28036e+11** |

### Filesystems, volumes & PVs

| Metric | Labels | Example |
|---|---|---|
| `talos_filesystem_available_bytes` | `mountpoint` | `mountpoint=/var` → **9.32383e+10** |
| `talos_filesystem_info` | `device`, `mountpoint` | `device=/dev/nvme0n1p4, mountpoint=/var` → **1** |
| `talos_filesystem_size_bytes` | `mountpoint` | `mountpoint=/var` → **1.25658e+11** |
| `talos_filesystem_used_bytes` | `mountpoint` | `mountpoint=/var` → **3.24199e+10** |
| `talos_pv_capacity_bytes` | `pv` | `pv=pvc-0908efb2-8a54-4d45-a74c-a0…` → **5.36871e+10** |
| `talos_pv_info` | `claim`, `driver`, `namespace`, `phase`, `pv`, `storageclass` | `claim=data-loki-ingester-0, driver=driver.longhorn.io, namespace=loki, phase=Bound, pv=pvc-e3a3be9a-af6a-45a0-9d91-6a…, storageclass=longhorn-1r` → **1** |
| `talos_volume_available_bytes` | `pv` | `pv=pvc-0908efb2-8a54-4d45-a74c-a0…` → **5.25007e+10** |
| `talos_volume_info` | `device`, `filesystem`, `pv` | `device=/dev/longhorn/pvc-332bfd77-e2d…, filesystem=block, pv=pvc-332bfd77-e2d9-4414-b1f0-f6…` → **1** |
| `talos_volume_size_bytes` | `pv` | `pv=pvc-0908efb2-8a54-4d45-a74c-a0…` → **5.25216e+10** |
| `talos_volume_used_bytes` | `pv` | `pv=pvc-0908efb2-8a54-4d45-a74c-a0…` → **2.08978e+07** |

### Exporter self-metrics

| Metric | Labels | Example |
|---|---|---|
| `talos_monitoring_k8s_nodes` | — | **4** |
| `talos_monitoring_k8s_persistentvolumes` | — | **24** |
| `talos_monitoring_node_scrape_errors_total` † | `collector`, `err` | `collector=block, err=other` → **0** |
| `talos_monitoring_collector_duration_seconds` | `collector` | `collector=pci` → **0.516** |
| `talos_monitoring_collections_total` † | `collector` | `collector=cpu` → **13** |
| `talos_monitoring_refresh_duration_seconds` | — | **0.0413** |

These answer the two questions that decide the deployment shape: **how much load is this
putting on the Talos API**, and **at what fleet size does one replica stop keeping up**.
Neither was answerable before — nothing measured the scheduler's own cost.

`talos_monitoring_collector_duration_seconds` is a gauge of the *last* observation, not a
histogram — the useful question is "which collector or node is slow right now", and a
histogram would cost buckets per (node, collector) pair. Failed collections get timed too,
since a collector that's slow because it's timing out is exactly the one worth seeing.

`talos_monitoring_refresh_duration_seconds` only covers ticks that actually collected
something. The scheduler runs every 1s while the fastest TTL is 5s, so most ticks are
near-instant and would drag the gauge to zero, hiding the cost of the rounds that did real
work. Measured on the reference cluster: **41ms** for a steady-state round across 4 nodes,
against a 30s scrape interval. Getting close to that interval means one replica is no
longer keeping up.

---

## Enforcement

These conventions are checked, not just written down. `internal/metricguard` turns them
into two tests every collector runs:

- **`Validate`** walks what a collector actually registered and flags a counter missing
  `_total`, an `_info` family carrying a measurement, a unit hiding in a label name, a
  family with no `node` label, or missing HELP text.
- **`SnapshotCovered`** enforces the "the UI shows nothing `/metrics` doesn't" rule for
  strings: every non-empty string a collector writes to the snapshot must also show up as
  a label value on a metric that same collector registered. This is what catches a value
  the dashboard can render but Prometheus can't — reverting the volume-error fix makes it
  fail with the exact message. Numbers are excluded on purpose: they're usually the
  metric's own value, and where they're not, they're unit-converted (MHz in the UI, hertz
  in the metric), so matching them would just be guesswork. Strings match exactly.

`TestEveryCollectorRunsTheGuard` closes the obvious hole here: the guard is opt-in per
collector, which is the same kind of problem it exists to solve — something correct that a
person has to remember to do. It fails if a collector writes to the snapshot but its tests
never call `metricguard`, and also if a collector on the `notYetWired` allowlist starts
running the guard — so the list can only shrink.

A fourth test, `TestEveryMetricIsDocumented`, scans the source for `talos_*` names and
this file for documented ones, and fails on drift either way. It caught two real bugs on
its first run: `talos_hw_pcidevice_numa_node_info` was exported but undocumented, and a
prose reference to a "talos_hw_pci_device_info" that never existed at all.

## Cardinality

### What drives it

Series scale with hardware, not with traffic — nothing here is per-request or per-PID.
Coefficients measured on the reference cluster (the model below reproduces its 2 247
series exactly):

| Per | Series | Notes |
|---|---|---|
| thread | **15.1** | 5.1 without `--collectors.cpu.mode-seconds`; that flag alone is 10 |
| network link | 15.3 | 8 counters, 2 rates, 5 state/identity |
| PCI device | 6.3 | identity, link speed/width, power state, AER, IRQ |
| disk | 6.0 | identity plus five measurements |
| volume / filesystem | 4.0 | size, used, available, identity |
| collector | 4.0 | zero-initialised error classes, per node |
| DIMM | 3.0 | identity, size, speed |
| sensor | 2.2 | reading, plus a threshold where the driver publishes one |
| PV | 2.0 | cluster-scoped, not per node |
| service | 3.0 | running, healthy, health_unknown — 11.5 services per reference node |
| extension entry | 1.0 | one `_info` series each, virtual entries included — 4.5 per reference node |
| kernel param | 2.0 | `_info` plus the unsupported flag — 31 per reference node |
| kernel module | 3.0 | `_info`, size, refcount — 18 per reference node |
| Talos volume | 2.0 | `_info` plus ready; +1 for the few with a capacity — 22 per node |
| mount | 3.0 | `_info`, read-only, quota — 20 per node |
| GPU card | 7.0 | identity, utilisation, VRAM pair, GTT pair, runtime-suspended |
| block device | 16.0 | identity plus 15 I/O counters — 11.25 per reference node, inflated by iSCSI PVs |
| node | 68.5 | memory, swap, identity, uptime, processes, sockets, machine stage/readiness, boot entry, cmdline, schematic, security posture, clock (9), load average (3), system disk, and the four zero-initialised counts |

### Estimates for real clusters

```
series ≈ nodes × (15.1·threads + 6.3·pci + 6.0·disks + 15.3·links
                  + 4.0·(volumes+filesystems) + 3.0·dimms + 2.2·sensors
                  + 3.0·services + 1.0·extensions
                  + 2.0·kernelparams + 3.0·kernelmodules
                  + 2.0·talosvolumes + 3.0·mounts + 7.0·gpus
                  + 16.0·blockdevices + 4.0·collectors + 68.5)
         + 2.0·pvs
```

| Cluster | Default | With `mode-seconds` | Prometheus RAM¹ |
|---|---|---|---|
| Homelab, 4 × 16t | **3 700** | 4 400 | ~11 MB |
| Small, 10 × 32t | **11 000** | 14 300 | ~32 MB |
| Medium, 50 × 32t | **56 000** | 72 000 | ~165 MB |
| Large, 200 × 64t | **278 000** | 406 000 | ~818 MB |

`kernel`, `volumes` and `diskio` dominate — 472, 433 and 720 series on the reference
cluster, against 36 for `time` and 25 for `gpu`. The estimates above assume 8 block
devices per node; the reference cluster has 11.25 because most of its disks are iSCSI
PersistentVolumes. If the series budget is tight, disabling those three
(`--collectors.<name>.enabled=false`) recovers roughly 400 series per node.

¹ at the usual ~3 KB of resident index and chunks per active series.

### Will this hurt Prometheus?

**Below ~50 nodes, no.** A single Prometheus is comfortable into the low hundreds of
thousands of active series; 34 000 from a 50-node fleet is a rounding error next to
`kube-state-metrics` and `node_exporter`, which typically contribute far more anyway.

**At 200+ nodes it becomes a real line item** — 187 000 series, 315 000 with per-mode CPU
time on. Still fine for one Prometheus instance, but no longer negligible, and worth a
deliberate decision rather than the default.

Three things worth knowing before you get there:

1. **`--collectors.cpu.mode-seconds` roughly doubles per-thread cardinality** (10 of the
   15.1 series per thread). That's exactly why it's off by default. Turn it on if you want
   `rate(...{mode="idle"})` in PromQL; otherwise use `talos_node_cpu_usage_ratio`.
2. **`--collectors.<name>.enabled` is the coarse control.** Dropping `pci` removes about
   6.3 series per device — roughly 30% of the total on hardware with a busy bus. Dropping
   `sensors` saves little (2.2 per sensor) but loses the most differentiated data the
   project has.
3. **Per-core cpufreq policy is 3 series per thread for one value.** `min`, `max` and
   `governor` are identical across every core on all four reference nodes. Collapsing them
   to node level would remove roughly 18 900 series at 200 nodes × 64 threads — a known,
   not-yet-done optimization.

**Label cardinality is bounded and static.** The widest label sets are on `_info` metrics
(14 labels on `talos_hw_pcidevice_info`, 15 on `talos_net_link_info`), but each is still just
one series per device, fixed by the machine. No label takes an unbounded value — the
highest-cardinality one is `bdf`, at about 26 per node. Nothing here depends on request
volume, pod count, or PID.

**Per-process data is left out on purpose.** A top-processes table would be unbounded
cardinality, so it stays UI-only if it's ever built — which is also why it hasn't been.

---

## Notes

**`talos_node_cpu_seconds_total` is opt-in** (`--collectors.cpu.mode-seconds`), for the
cardinality reason above. With it on:

```promql
100 * (1 - rate(talos_node_cpu_seconds_total{mode="idle"}[5m]))
```

**Two metrics are rates the exporter computes itself**, over its own scrape interval
rather than a window you choose: `talos_node_cpu_usage_ratio` and
`talos_net_link_{rx,tx}_bytes_per_second`. They exist because the dashboard needs them and
the counter behind the CPU one is opt-in. For querying, prefer `rate()` on the counter
instead.

**`talos_volume_used_bytes` and `talos_filesystem_used_bytes` are `size − available`.**
Talos only reports size and available. That matches `df`'s Used figure only where the
filesystem has no reserved blocks — true of the CSI volumes on the reference cluster.

**`talos_hw_processor_status`** is the raw SMBIOS status word: bit 6 means socket
populated, bits 0–2 give CPU status (1 enabled, 2 disabled by user, 3 disabled by BIOS,
4 idle). `65` means populated and enabled. `_socket_populated` and `_enabled` decode the
two bits worth alerting on.

**Sensor thresholds are whatever the driver publishes.** Some are placeholders — NVMe
reports `min = -273.15°C`, absolute zero, meaning "no minimum". The UI reads `critical`
then `max` and ignores the rest.

**`talos_node_cpu_freq_hertz` reads high.** The scrape itself is load, and cpufreq
governors react in microseconds — measured +2.9% on an idle 16-thread node, +121% on a
busy 4-thread one. It's sampled first in the collection to minimise this, but prefer a
range or quantile over a window to trusting any single reading.

**Cluster-scoped metrics carry no `node` label:** `talos_pv_info`,
`talos_pv_capacity_bytes`, `talos_monitoring_k8s_nodes`,
`talos_monitoring_k8s_persistentvolumes`.

**Sensitive labels.** `/metrics` carries chassis serials, system UUIDs, DIMM serials and
disk WWIDs, and is unauthenticated. Keep it cluster-internal.

## Regenerating

The tables come from a live exposition, not from hand-editing:

```bash
./bin/talos-monitoring serve --listen :8095 --collectors.cpu.mode-seconds
curl -s localhost:8095/metrics   # parse: name, type, labels, one example series
```
