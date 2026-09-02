# Changelog

## Unreleased

### Added

- `storage.backup`: an optional PVC (`<instance-name>-backup`), mounted into the pod at
  `/var/lib/firebird/backup`, for staging backups. Like the primary/shadow PVCs, it is never
  owner-referenced to the `Instance`.
- Deleting an `Instance` with `storage.backup` configured now backs up every database in
  `status.databases` into a subdirectory of the backup volume dedicated to that `Instance`
  (`<mount>/<instance-name>/<database>.fbk`, via `gbak -backup -verify`) before removing its
  finalizer, then releases the primary and shadow PVCs (deleting them outright, unlike the
  default behavior of leaving all storage in place) — the backup PVC itself is left untouched.
  The dedicated per-instance subdirectory keeps backups from different `Instance`s, or from
  successive generations of one reusing the same backup PVC, from colliding.
- Recreating an `Instance` closes that loop: for a database not already on the primary PVC,
  `storage.backup` is checked for a matching `<instance-name>/<database>.fbk` backup, and if one
  is there it's restored via `gbak -create -verify` (recreating the shadow file too, for a
  `shadow: true` database) instead of creating an empty database — so an `Instance` deleted with
  `storage.backup` configured can be fully recreated from its own backup PVC.
- `status.message` now tracks deletion progress instead of showing a stale pre-deletion value:
  `"Deleting Instance"`, then (when `storage.backup` is set) `"Waiting for the Firebird pod to be
  ready before backing up databases"` if it has to block, `"Backing up databases into
  storage.backup"`, and `"Releasing primary and shadow storage"`, then `"Removing finalizer"`.

### Fixed

- Reusing a primary PVC left behind by an earlier `Instance` of the same name (PVCs are never
  owner-referenced, so a later `Instance` reusing the name reuses the PVC too) no longer fails:
  `reconcileDatabases` now checks whether a pending database's file already exists on it before
  running `CREATE DATABASE`, and just registers it into `status.databases` — alongside its
  already-unconditional `databases.conf` alias — instead of re-creating (and risking clobbering)
  it.

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
