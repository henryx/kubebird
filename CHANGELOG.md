# Changelog

## Unreleased

### Added

- `spec.backup`: configures backing up the instance's databases. `spec.backup.enabled` turns on the
  ability to back up, but by itself creates nothing — a dedicated PVC (`<instance-name>-backup`),
  mounted into the pod at `/var/lib/firebird/backup` and sized via
  `spec.backup.destinations[].local.storage`, is only actually created when
  `spec.backup.destinations` also has a `local` entry (the only backup destination currently
  implemented; `destinations` is a list so others can be added later without a breaking change).
  The CRD rejects `spec.backup.enabled: true` with an empty `spec.backup.destinations`.
  Like the primary/shadow PVCs, the backup PVC is never owner-referenced to the `Instance`.
- Deleting an `Instance` now always deletes its primary PVC, and its shadow PVC if `storage.shadow`
  was set, regardless of `spec.backup` — a behavior change from always leaving all storage in
  place. Without a local backup volume configured, this permanently destroys the `Instance`'s data.
- `spec.backup.backupOnDelete` (defaults to `true`) controls whether deletion runs that final backup
  at all: set to `false`, deletion skips straight to releasing the primary/shadow storage, leaving
  an already-existing backup PVC as-is (whatever an earlier backup left in it) rather than adding a
  fresh one. Has no effect without a local backup volume configured, since there's nothing to back
  up into either way.
- Whenever a local backup volume exists and `spec.backup.backupOnDelete` is `true`, deletion backs up every database in `status.databases`
  into a fixed `base` subdirectory of the backup volume (`<mount>/base/<database>.fbk`, via
  `gbak -backup -verify`) before releasing the primary/shadow PVCs, and always leaves the backup
  PVC in place afterward — there's no way to opt out of retaining it once it exists. `base` doesn't
  need to be named after the `Instance`: the backup volume is already a PVC dedicated to that
  `Instance` (named `<instance-name>-backup`), so a fixed subdirectory name avoids stuttering the
  name into the path a second time. `gbak` can't back up a database that a live server still has
  open, so this first stops the pod (scaling the StatefulSet to 0 replicas) and runs every backup —
  including the security database's, see below — from a short-lived helper Pod that mounts the same
  primary, backup, and (when configured) shadow PVCs instead, deleting the Pod again once it
  succeeds. Without a local backup volume, there's no backup PVC to consider, and the primary/shadow
  PVCs are released immediately with no pod restart.
- Recreating an `Instance` closes that loop, but only when a local backup volume was configured
  before deletion: for a database not already on the (now fresh) primary PVC, the backup volume is
  checked for a matching `base/<database>.fbk` backup, and if one is there it's restored via
  `gbak -create -verify` (recreating the shadow file too, for a `shadow: true` database) instead of
  creating an empty database — so an `Instance` deleted with a backup volume can be fully recreated
  from its own backup PVC. This also covers upgrading between Firebird major versions: recreating
  with a different `spec.version` restores the backup taken by the old version's `gbak` using the
  new version's own `gbak` and server, verified end-to-end (backup, delete, recreate with a bumped
  `spec.version`, then confirm a table created before deletion is still there). Without a backup
  volume, a recreated `Instance` starts fresh instead.
- `status.message` now tracks deletion progress instead of showing a stale pre-deletion value:
  `"Deleting Instance"`, then (when backing up) `"Stopping the Firebird pod to back up its
  databases"` while it does that, `"Backing up databases into the backup volume"` while the helper
  Pod runs, `"Releasing primary and shadow storage"`, then `"Removing finalizer"`.
- `status.warning`: a non-fatal notice, surfaced through `status.message`/the `MESSAGE` printer
  column, that the backup volume holds a `.fbk` for a database no longer in `spec.databases` — e.g.
  because the `Instance` was recreated with a different database list, or a database was dropped
  after its backup was taken — so it was left unrestored instead of silently ignored. Cleared once
  no orphaned backup remains.
- The security database (`securityN.fdb`) now lives on the primary PVC (`/var/lib/firebird/data`)
  instead of the image's own ephemeral install directory, so it survives a pod restart. A new
  `security-database-init` init container seeds it there — on every fresh primary PVC, since
  deleting an `Instance` always deletes the previous one — from the image's own default the first
  time, before the `firebird` container starts; the `security.db` alias in `databases.conf` and a
  new `FIREBIRD_CONF_SecurityDatabase` environment variable both point the live engine at this same
  relocated path.
- Whenever a local backup volume exists, deleting an `Instance` now also backs up the security
  database itself (into `<mount>/base/securityN.fbk`, via the same offline `gbak -backup -verify`
  described above) before releasing the primary PVC that carries it, and `security-database-init`
  restores that backup — via its own local `gbak -create -verify` — in preference to the image's
  stock default when the `Instance` is recreated under the same name — so users/roles created
  directly in the security database now survive a delete/recreate cycle the same way application
  data in `spec.databases` already did (both only with a backup volume configured — see above).
  This backup runs unconditionally whenever it runs at all, even when `status.databases` is empty,
  since the security database always exists regardless of `spec.databases`.
- `spec.backup.image`: optionally overrides the image (including tag) used to run the short-lived
  helper Pod that backs up an `Instance`'s databases on deletion. Defaults to
  `<spec.image>:<spec.version>` — the same image the `firebird` container and the
  `security-database-init` init container run — when left unset.
- Rotating the SYSDBA Secret's password now restarts the pod to apply it: the `StatefulSet`'s pod
  template carries a `kubebird.github.io/sysdba-password-hash` annotation hashing the Secret's
  current password, so a rotation changes the template and the `StatefulSet` controller's own
  rolling update recreates the pod, letting the image's entrypoint apply the new password the same
  way it already does for a brand-new `Instance` — rather than Kubebird pushing the change to the
  live server itself.
- `spec.backup.retention`: recurring scheduled backups, independent of `backupOnDelete` and running
  for as long as the `Instance` exists rather than only at deletion. Its five fields (`hour`,
  `day`, `week`, `month`, `year`) are each both the switch for that frequency (empty, the default,
  or a zero duration disables it) and how long its backups are kept, as a time-based
  `<n><unit>` duration — `h` (hours), `d` (days), `w` (weeks), `m` (months), `y` (years); e.g.
  `day: 3d` takes a daily backup and deletes that frequency's backups older than three days. The
  CRD rejects any other format. Has no effect without a local backup volume
  configured, same as `backupOnDelete`.
- Each enabled frequency gets its own native `batch/v1` `CronJob` (`<name>-backup-<frequency>`), on
  a fixed schedule matching its name (hourly on the hour, daily at 00:00 UTC, weekly at 00:00 UTC
  on Sunday, monthly at 00:00 UTC on the 1st, yearly at 00:00 UTC on January 1st) — removed again if
  that frequency's retention is later unset (or set to a zero duration). Kubernetes' own CronJob controller drives each
  one's actual run schedule; Kubebird doesn't track or poll individual runs itself.
  `ConcurrencyPolicy: Forbid` skips a run outright rather than queueing or overlapping it with one
  still in flight, and `BackoffLimit: 0` means a failed run isn't retried either — both cases just
  wait for the next scheduled tick instead. Keeps the last successful Job and the last 3 failed ones
  (`SuccessfulJobsHistoryLimit`/`FailedJobsHistoryLimit`) for troubleshooting.
- Each run backs up every database in `status.databases`, using the same physically-consistent,
  guarded-lock backup `nbackup` performs — but reached through the Services API
  (`fbsvcmgr ... -action_nbak`) against the instance's own already-running server, since `nbackup`
  itself only operates on a database local to wherever it runs. Per the Services API's own
  conventions, both the source database and the backup file are resolved on the server, so the
  backup lands directly in the backup volume already mounted into the live `firebird` container —
  the CronJob's Job reaches the server over the network, through the instance's `Service` with the
  SYSDBA credentials from the Secret, and mounts only the backup PVC, to prune expired backups (see
  below). Since that PVC is `ReadWriteOnce`, the Job has a required pod affinity to the Firebird
  pod's own node. The `Service`'s selector (and the
  `StatefulSet`'s own `Selector`/pod template) requires an `app.kubernetes.io/component: firebird`
  label that only the actual Firebird pod carries, so the Job's own pod is never routed to as a
  broken Endpoint despite sharing the rest of the `Instance`'s labels. Unlike `backupOnDelete`'s
  `gbak` backup, this never stops the pod. The security database is deliberately excluded: unlike `spec.databases`,
  it can't be `nbackup`'d while the server has it open, neither locally nor remotely through the
  Services API, so it stays covered only by the `backupOnDelete` backup taken on deletion. Each
  backup is written as `<frequency>/<database>-<timestamp>.nbk`, where `<timestamp>` is the run's
  own UTC start time (`YYYYMMDDTHHMMSSZ`), so every run writes a new file and never collides with an
  earlier one. Once all of a run's backups have succeeded, the same Job deletes every backup in that
  frequency's directory whose file name timestamp is older than the retention (via GNU `date -d
  "<n> <unit> ago"`, so months and years follow the calendar); a failed run exits before pruning, so
  it never deletes older backups.
- Every enabled frequency's own backup directory is periodically checked (every 5 minutes) for a
  `.nbk` file not yet gzip-compressed, and compressed in place (`exec`'d into the live pod, since
  neither `nbackup` nor `fbsvcmgr` can compress their own output) to
  `<frequency>/<database>-<timestamp>.nbk.gz` — independently of any specific run's own lifecycle,
  since Kubebird doesn't otherwise observe a CronJob-spawned Job's completion.
- New RBAC marker (`get;list;watch;create;update;patch;delete` on `batch`'s `cronjobs`) for the
  scheduled-backup CronJobs above.

### Changed

- The SYSDBA Secret is no longer owner-referenced to the `Instance`, so it now survives deleting
  the `Instance` — like the backup PVC, but unlike the primary/shadow PVCs, which are never
  owner-referenced either but are explicitly deleted by the controller itself on every deletion —
  instead of being garbage-collected along with it; recreating an `Instance` under the same name
  reuses that same Secret and its password rather than generating a new one. When
  `spec.authentication.sysdba.secretRef` is set, that Secret must already exist — reconciliation now
  fails with an error if it's missing instead of auto-creating it; the default
  `<instance-name>-sysdba` Secret is still created automatically when it doesn't already exist.

### Fixed

- `reconcileDatabases` now checks whether a pending database's file already exists on the primary
  PVC before running `CREATE DATABASE`, and just registers it into `status.databases` — alongside
  its already-unconditional `databases.conf` alias — instead of re-creating (and risking
  clobbering) it. This guards a crash-recovery window: a previous reconcile may have already run
  `CREATE DATABASE` and then failed before recording it in `status.databases`.
- The generated SYSDBA password could occasionally start with `-`, which broke any tool invoked
  with it as a bare `-password <value>` CLI argument (e.g. `isql`, misparsing the leading `-` as a
  flag of its own rather than part of the password). `generateRandomPassword` now rejects that
  case and generates another password instead.

## 0.2.0

Rewrite of the operator from Python/`kopf` to Go, scaffolded with Kubebuilder v4
(`go.kubebuilder.io/v4`, single-group layout). The `Instance` CRD (`kubebird.github.io/v1`) and
its spec shape are unchanged; this section tracks where the implementation differs from the
`0.1.0` Python operator below.

### Added

- `Instance` lifecycle driven by a `controller-runtime` `Reconcile` loop instead of separate
  `create_fn`/`update_fn`/`delete_fn` handlers, guarded by a `kubebird.github.io/finalizer`
  finalizer so deletion is observed before owner-referenced resources are garbage-collected.
- Databases are created/dropped by `exec`-ing `isql` inside the `firebird` container (via
  `client-go`'s `remotecommand`) rather than through a Go database driver, so `pageSize`,
  `charset`, and `collation` are always applied faithfully instead of being silently overridden.
- SYSDBA password rotation execs `isql` against the security database file directly (an
  embedded, OS-trusted connection) instead of shelling out to `gsec`, and is triggered by a
  `Watches(&corev1.Secret{}, ...)` mapped through a field indexer on `secretRef`, so it fires as
  soon as the Secret changes rather than on a polling interval.
- `status.databases` tracks which databases have been created, making both creation and
  drop idempotent across reconciles; `status.databaseCount` (`len(status.databases)`) is
  exposed as its own printer column since CRD `JSONPath`s can't apply a length function.
- `status.phase` (`Provisioning`/`Ready`/`Deleting`), `status.error`, and `status.message` are
  exposed as `Status`/`Message` printer columns alongside `Version` and `Databases`.
- Primary/shadow PVCs are deliberately **not** owner-referenced to the `Instance` (unlike every
  other resource Kubebird creates), so an `Instance`'s data now survives deleting the `Instance`
  unless the PVCs are removed directly — a behavior change from the Python operator's blanket
  `kopf.adopt()`.
- Database aliases are registered via a ConfigMap mounted with `SubPath` directly at
  `/opt/firebird/databases.conf`, rather than a writable `emptyDir`; because a `SubPath` mount
  replaces that file wholesale, a `security.db` alias is also appended so the image's own
  default alias for the security database isn't dropped.
- RBAC, CRD, and Deployment manifests are generated via `make manifests` (Kubebuilder markers in
  `internal/controller/instance_controller.go`) instead of a hand-maintained
  `deploy/operator.yaml`; `make build-installer` emits a consolidated `dist/install.yaml`.
- GitHub Actions workflows split into `lint.yml`, `test.yml` (envtest/Ginkgo), `test-e2e.yml`
  (against a Kind cluster), and `release.yml` (triggered by pushing a semver tag, publishing to
  `quay.io/kubebird/operator` and attaching `install.yaml` to a GitHub Release).
- `dev-image.yml` builds and pushes `quay.io/kubebird/operator:dev` on every push to `main`,
  giving a rolling image that tracks `main` without cutting a versioned release or GitHub
  Release.
- `release.yml` and `dev-image.yml` both call `lint.yml`, `test.yml`, and `test-e2e.yml` as
  reusable workflows and gate their build/publish job on all three succeeding
  (`needs: [lint, test, test-e2e]`), so a tag or `main` push that fails lint, tests, or e2e
  tests never reaches `quay.io` or cuts a release.
- The `firebird` container sets `allowPrivilegeEscalation: false` and a `RuntimeDefault` seccomp
  profile, satisfying the `baseline` Pod Security Standard (it still runs as root, since the
  `firebirdsql/firebird` entrypoint needs full DAC override whenever `FIREBIRD_ROOT_PASSWORD` is
  set, which Kubebird always does — so it cannot satisfy `restricted`).
- Manager configuration moved from `NAMESPACE`/`LOG_LEVEL` environment variables to a required
  `WATCH_NAMESPACE` environment variable (single or comma-separated namespaces) plus standard
  `controller-runtime` flags (e.g. `--zap-log-level`) for logging.

## 0.1.0 (2026-08-13)

Initial release.

### Added

- `Instance` custom resource (`kubebird.github.io/v1`) and its full lifecycle: `create_fn`,
  `update_fn`, and `delete_fn` handlers built on `kopf`.
- Deploys a Firebird instance as a `StatefulSet` (1 replica), using the `image`/`version` given
  in `spec`.
- Creates a `Service` for the instance (`spec.service.type`, default `ClusterIP`), exposing
  `spec.service.port` (default `3050`).
- Provisions PVCs for primary storage (`storage.primary`, required) and shadow storage
  (`storage.shadow`, required only when a database uses `shadow: true`).
- Creates each database listed in `spec.databases` via `isql`, with configurable `pageSize`
  (`4096`/`8192`/`16384`, default `8192`), `charset`, and `collation` (both default `UTF8`), and
  optional shadow-file creation (`CREATE SHADOW`).
- Registers a Firebird alias per database in `/opt/firebird/databases.conf` (via a ConfigMap +
  writable `emptyDir`), so clients can connect by alias instead of the in-pod path.
- Manages SYSDBA authentication: auto-generates a `<instance-name>-sysdba` Secret
  (`username`/`password` keys) when `authentication.sysdba.secretRef` isn't set, or reads a
  user-provided secret otherwise.
- Labels every created object with `kubebird.github.io/instance: <name>` for easy discovery via
  `kubectl get all,pvc,secrets -l ...`.
- Adopts every created object (`kopf.adopt()`) so deleting the `Instance` garbage-collects them
  automatically.
- Reports reconciliation progress via `status.phase`/`status.message`, and any handler failure
  into `status.error`, surfaced through the CRD's `Error` printer column.
- `update_fn` reconciles `spec.service.type`/`spec.service.port`/`spec.version` changes and
  provisions newly-added entries in `spec.databases` without restarting the pod.
- `sysdba_secret_update_fn` watches the SYSDBA `Secret` and pushes a rotated password to the live
  server via `gsec`, keeping the secret and the running instance in sync.
- `delete_fn` supports clean deletion of an `Instance` and all its owned resources.
- RBAC manifest (`deploy/operator.yaml`) with namespace-scoped `Role`/`RoleBinding` and a thin
  cluster-scoped `ClusterRole`/`ClusterRoleBinding` for kopf's framework requirements.
- `Dockerfile` for building the operator into a container image, running as a non-root user.
- `kubebird-operator` console script entry point, running on `uvloop`, with `NAMESPACE` and
  `LOG_LEVEL` environment variable support.
- GitHub Actions CI workflow (lint, format, type-check, tests with coverage) and a release
  workflow publishing container images to `quay.io/kubebird/operator`.
- Test suite covering create/update/delete flows end-to-end against a real k3s cluster
  (via `testcontainers`), plus RBAC verification.
