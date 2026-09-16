# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project overview

Kubebird is a Kubernetes operator, scaffolded with Kubebuilder v4 (go.kubebuilder.io/v4, single-group layout), that installs and manages [Firebird RDBMS](https://firebirdsql.org/) instances via a namespaced `Instance` custom resource in API group `kubebird.github.io/v1`.

`InstanceSpec` (`api/v1/instance_types.go`) and the `Reconcile()` loop (`internal/controller/instance_controller.go`) implement the shape documented in README.md:

```yaml
apiVersion: kubebird.github.io/v1
kind: Instance
metadata:
  name: test
spec:
  image: firebirdsql/firebird
  version: 3.0.14
  databases:
    - name: "instance.fdb"
      shadow: false
      pageSize: 8192 # defaults to 8192; one of 4096, 8192, 16384
      charset: UTF8 # defaults to UTF8
      collation: UTF8 # defaults to UTF8
    - name: "shadowed.fdb"
      alias: "enforced" # if not specified, uses database name as alias
      shadow: true
  service:
    type: ClusterIP
    port: 3050 # port the Service exposes the instance on; defaults to 3050
  storage:
    primary:
      class: "" # if empty, uses the default storage class
      size: 3Gi
    shadow: # can be omitted if no database below has "shadow: true"
      class: ""
      size: 3Gi
  authentication: # optional; if omitted, a "<name>-sysdba" secret is generated
    sysdba:
      secretRef: "" # if empty, defaults to "<name>-sysdba"
  backup: # backup section
    enabled: true # enable or disable backup
    type: # whenever a local entry is present, deletion always backs up and preserves the PVC
      - local: # use a dedicated PVC
          storage:
            class: ""
            size: 3Gi
```

Treat the README sample as the source of truth for spec shape, and check with the user before diverging from it.

## Commands

```bash
make manifests generate   # after editing api/v1/*_types.go: regen CRDs/RBAC + DeepCopy methods
make fmt vet               # format and vet
make lint                  # golangci-lint (config: .golangci.yml)
make lint-fix               # golangci-lint --fix
make test                  # unit/envtest suite (runs manifests generate fmt vet setup-envtest first)
make test-e2e               # e2e suite against a Kind cluster (creates/tears down $KIND_CLUSTER, default "kubebuilder-test-e2e")
make run                   # run the manager locally against the current kubeconfig context
make build-installer        # regen manifests + emit dist/install.yaml (CRD+RBAC+Deployment); IMG defaults to quay.io/kubebird/operator:latest
```

Run a single test (Ginkgo/Gomega BDD style, under envtest):

```bash
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" go test ./internal/controller/... -run TestControllers -coverprofile cover.out
# or scope to one spec via Ginkgo focus, e.g. add an `F` prefix (FIt/FDescribe) or:
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" go test ./internal/controller/... --ginkgo.focus="should reconcile the Service, ConfigMap and StatefulSet"
```

`make test` excludes `./test/e2e/...` (`go list ./... | grep -v /e2e`). e2e tests are build-tagged `e2e` and require a Kind cluster — never point `test-e2e` at a real dev/prod cluster.

## Architecture

Standard Kubebuilder single-group layout — see `AGENTS.md` in this repo for the full scaffolding reference (file layout, marker conventions, RBAC markers, CLI commands for adding APIs/webhooks). Key points specific to this repo:

### Scaffolding commands

Recorded in `PROJECT` — never hand-edit that file either. The project was bootstrapped with `kubebuilder init --domain github.io --repo github.com/henryx/kubebird` (kubebuilder v4.15.0, single-group layout, namespaced), then the `Instance` CR was added with `kubebuilder create api --group kubebird --version v1 --kind Instance --resource --controller`. No `kubebuilder create webhook` has been run for `Instance` — `PROJECT` has no webhook entry and there's no `api/v1/instance_webhook.go`. Use the same two commands (never hand-write scaffolding) for any future API or webhook — see `AGENTS.md`'s CLI Commands Cheat Sheet for the full flag reference.

### API group wiring

`PROJECT` declares `group: kubebird`, `domain: github.io`; these compose to `kubebird.github.io`, which must stay consistent across `api/v1/groupversion_info.go` (`+groupName`), the RBAC markers in `internal/controller/instance_controller.go`, and any sample/manifest YAML. A mismatch here (e.g. a doubled `kubebird.github.io.github.io`) breaks the CRD/manifest wiring silently.

### Generated vs. owned files

`api/v1/zz_generated.deepcopy.go`, `config/crd/bases/*.yaml`, and `config/rbac/role.yaml` are produced by `make manifests generate` — never hand-edit them. `api/v1/instance_types.go` and `internal/controller/*.go` are the owned files to extend.

### envtest CRD source

`internal/controller/suite_test.go` loads CRDs from `config/crd/bases/` for the test API server (`ErrorIfCRDPathMissing: true`), so `make manifests` must be run (and the CRD committed/present) before `make test` will pass.

### Namespace scoping

`cmd/main.go` requires a `WATCH_NAMESPACE` env var and configures the manager's cache to watch only that namespace (or comma-separated list) via `setupCacheNamespaces` — the manager will not start without it. Don't confuse this with where the operator itself runs: `config/default/kustomization.yaml` deploys the manager, RBAC and Service into the `kubebird-system` namespace (via `namespace:`/`namePrefix: kubebird-`), which must stay paired — kustomize requires `namePrefix` to match the text before the first `-` in `namespace`. The `app.kubernetes.io/name: kubebird` label/selector pair (`config/manager/manager.yaml`, `config/default/metrics_service.yaml`, `config/network-policy/allow-metrics-traffic.yaml`, `config/prometheus/monitor.yaml`) must also change together — it's how the metrics Service, NetworkPolicy and ServiceMonitor find the manager Pod.

### Release/publishing

`.github/workflows/release.yml` triggers only on pushing a semver tag (`[0-9]+.[0-9]+.[0-9]+`, e.g. `0.2.0`) — it builds/pushes the manager image to `quay.io/kubebird/operator` (versioned + `latest`) and attaches `dist/install.yaml` to a GitHub Release. `.github/workflows/dev-image.yml` triggers on every push to `main` and just builds/pushes `quay.io/kubebird/operator:dev` — no versioned tag, no `latest`, no GitHub Release — giving a rolling image that tracks `main` for testing unreleased changes. Both workflows call `lint.yml`, `test.yml` and `test-e2e.yml` as reusable workflows (`uses: ./.github/workflows/*.yml`, invoked via the `workflow_call` trigger added to each) and gate their build/publish job on all three succeeding (`needs: [lint, test, test-e2e]`) — a tag or `main` push that fails lint, unit/envtest, or e2e tests never reaches quay.io or cuts a GitHub Release. `Makefile`'s `IMG` default and `config/manager/kustomization.yaml`'s `images:` transform both point at that same registry, so `make build-installer`/`make docker-build docker-push` without an explicit `IMG=` also target `quay.io/kubebird/operator`.

### Logging convention

This repo follows the Kubernetes logging style guide (capitalized message, no trailing period, active/past voice, object type named explicitly) — enforced in part by the custom `logcheck` golangci-lint module in `.golangci.yml`.

### Reconcile flow

`internal/controller/instance_controller.go`, split across `instance_resources.go` and `instance_provision.go`. Every object Kubebird creates is labelled `kubebird.github.io/instance: <name>`. For each `Instance`, the controller runs the steps below in order.

Note on `spec.backup.enabled`: throughout this section, wherever behavior is described as gated on `spec.backup.enabled` being true, the actual condition every one of these call sites checks in code is `backupVolumeSpec(instance) != nil` — `spec.backup.enabled` is true *and* `spec.backup.type` has a `local` entry (the only backup destination currently implemented). `spec.backup.enabled` alone only turns on the ability to back up; it doesn't by itself create or imply a backup PVC.

#### 1. SYSDBA Secret

When `spec.authentication.sysdba.secretRef` is set, that Secret must already exist — it's the user's own, created ahead of time, and reconciliation fails with an error if it's missing rather than auto-creating it. When unset, the default `<name>-sysdba` Secret is created (with a generated random password and a `username: SYSDBA` key) if it doesn't exist yet. Either way, an existing Secret is never touched. The Secret is deliberately never owner-referenced to the `Instance` (`reconcileSysdbaSecret` skips `controllerutil.SetControllerReference`, the same way `reconcilePVC` skips it for the storage PVCs — see "Finalizer and deletion" below), so it survives deleting the `Instance` rather than being garbage collected along with it, and a later `Instance` recreated under the same name (whether via the default name or an explicit `secretRef` pointing at the same Secret) reuses it and its password as-is.

#### 2. Core resources

Reconciles a `ConfigMap` registering one Firebird alias per `spec.databases[]` entry (`alias` if set, else the database name) in a `databases.conf` mounted (via `SubPath`) at `/opt/firebird/databases.conf`, a `Service`, PVCs named `<name>-primary`/`<name>-backup`/`<name>-shadow` from `spec.storage` and `spec.backup` (created by `reconcilePVC`/`reconcilePVCs` if missing — `get;list;watch;create` only, size/class are effectively immutable once created, same as the volumeClaimTemplates approach it replaced; the backup PVC's storage comes from `spec.backup.type[].local.storage`, and is only created/mounted when `spec.backup.enabled` is true *and* `spec.backup.type` has a `local` entry (`spec.backup.enabled` alone only turns on the ability to back up, it doesn't by itself create the PVC) — see `backupVolumeSpec`; `storage.shadow` is likewise optional and only created/mounted when set), and a single-replica `StatefulSet` running the `firebirdsql/firebird` image, which mounts those PVCs directly by claim name rather than via `spec.volumeClaimTemplates` (the backup PVC, when present, is mounted at `/var/lib/firebird/backup`; Kubebird itself only writes to it as the last step of deletion — see "Backup-and-release on deletion" below).

- The `firebird` container's `SecurityContext` sets `allowPrivilegeEscalation: false` and a `RuntimeDefault` seccomp profile (satisfying the `baseline` Pod Security Standard) but leaves `RunAsNonRoot`/capabilities untouched, since the image's entrypoint needs root's full DAC override whenever `FIREBIRD_ROOT_PASSWORD` is set, which Kubebird always does — so `Instance` pods can't satisfy `restricted`, and the namespace they run in must enforce `baseline` or looser.
- The security database (`securityN.fdb`) is kept on the primary PVC (`/var/lib/firebird/data/securityN.fdb`, via `securityDatabasePath`) rather than the image's own ephemeral `/opt/firebird`. A `security-database-init` initContainer (same image, mounting the primary PVC, plus the backup PVC too when `spec.backup.enabled` is true) runs before the `firebird` container and seeds it there, if it isn't already present — since `releasePrimaryAndShadowStorage` always deletes the primary PVC on `Instance` deletion (see "Finalizer and deletion" below), the normal case is a brand-new PVC on every create or recreate; the check only ever finds one already there across a plain pod restart within the same `Instance`'s lifetime — from, in preference order: its own `gbak` backup left behind by `backupDatabases` at `<backup mount>/base/securityN.fbk` (`instanceBackupDir`'s fixed `base` subdirectory — not named after the `Instance`, since the backup volume is already a PVC dedicated to it; see "Backup-and-release on deletion" below), restored via a local `gbak -create -verify` (no host given, so it runs against the image's own local engine directly — the only option anyway, since no firebird server is listening yet at this point in the Pod's startup; confirmed against the actual image that this needs no `-user`/`-password`, since there's no pre-existing security database yet for it to authenticate the connection against), if `spec.backup.enabled` is true and one exists there; otherwise the image's baked-in default (`securityDatabaseImageDefaultPath`), seeded with a plain `cp` since that source is a raw database file, not a `gbak` archive (`securityDatabaseInitScript`). The `firebird` container itself gets a `FIREBIRD_CONF_SecurityDatabase` env var (a documented image override) pointing the engine at that same relocated path.
- Because a `SubPath` mount replaces that file wholesale rather than merging with the image's own copy, `mutateAliasesConfigMap` also appends a `security.db` alias for the security database (`RemoteAccess = false`) so the image's default alias for it isn't silently dropped — pointed at the same relocated `securityDatabasePath`, not the image's built-in `$(dir_secDb)` macro, since that macro is fixed to the image's install root and doesn't follow the `FIREBIRD_CONF_SecurityDatabase` override above. The image's own entrypoint applies `FIREBIRD_ROOT_PASSWORD` by connecting to this same alias (`isql -user SYSDBA security.db`), so it follows the relocation too.

#### 3. SYSDBA password sync

`mutateStatefulSet` stamps the pod template with a `kubebird.github.io/sysdba-password-hash` annotation hashing the SYSDBA Secret's current password on every reconcile. Rotating the Secret changes that hash, which changes the template, which the StatefulSet controller's own rolling update picks up by deleting and recreating the (single-replica) pod — the same path a brand-new `Instance` already takes to get `FIREBIRD_ROOT_PASSWORD` applied by the image's own entrypoint at container start. Kubebird doesn't push the new password to a live server itself: there's no way to do that without either the outgoing password (gone from Kubernetes state the moment the Secret is overwritten) or an OS-trusted local connection to the security database, and the latter conflicts with the live server's own already-open engine instance on that file ("Database already opened with engine instance, incompatible with current") — a restart sidesteps the problem entirely rather than working around it. The cost is a full pod restart (briefly unavailable to every connection) instead of the pod staying up throughout, but it needs no direct exec into the container and no special-casing around the security database.

#### 4. Database provisioning

`exec`s `isql` inside the `firebird` container (via `client-go`'s `remotecommand`, requiring the `pods/exec` RBAC verb) to run `CREATE DATABASE`/`CREATE SHADOW` for each entry in `spec.databases` not yet recorded in `status.databases`, and `DROP DATABASE` for each entry in `status.databases` no longer present in `spec.databases`:

- **Create** — except when `databaseFileExists` (an `exec`ed `test -f`, checked first) finds the database's file already on the primary PVC, in which case it's just added to `status.databases` without re-running `CREATE DATABASE`. This guards a crash-recovery window, not PVC reuse — since `releasePrimaryAndShadowStorage` always deletes the primary PVC on `Instance` deletion (see "Finalizer and deletion" below), the only way this legitimately finds a pre-existing file is a previous reconcile having run `CREATE DATABASE` and then failed before `status.databases` was updated to record it.
- **Restore** — otherwise, if `spec.backup.enabled` is true, `restoreDatabaseIfBackedUp` checks for a backup at `<backup mount>/base/<database>.fbk` (the same layout `backupDatabases` writes) and, if found, `exec`s `gbak -create -verify` to restore it — then, for a `shadow: true` database, a follow-up `isql` session connected directly to the restored file (`isql -user ... <dbpath>`) re-adds the shadow via `CREATE SHADOW 1 '<path>'`, since gbak's restore doesn't recreate one — instead of falling through to `CREATE DATABASE`. This also works across a Firebird major-version bump: `gbak` restores are forward-compatible, so a `.fbk` backed up by an older `spec.version`'s `gbak` restores cleanly under a recreated `Instance`'s newer `spec.version` and `gbak`.
- **Drop** — via the `databaseDropScript` constant, isql connecting by passing the database's path as its positional argument (Firebird drops any attached shadow along with it). Removing a `spec.databases` entry also drops its alias from the next `databases.conf` regeneration, since `mutateAliasesConfigMap` rebuilds that file from `spec.databases` alone on every reconcile.
- Databases are provisioned this way — not via a mounted init script — specifically because the `nakagami/firebirdsql` Go driver's database-creation path hardcodes `page_size=4096` and always sets `isc_dpb_overwrite`, which would silently ignore `spec.databases[].pageSize` and risk clobbering an existing database file; `isql` over `exec` preserves full fidelity to the CRD fields. `status.databases` is what makes both directions idempotent — a database already listed there is never re-created, and one no longer listed is never re-dropped.

#### 5. Orphaned backup warnings

Whenever there's at least one pending database and `spec.backup.enabled` is true, `reconcileDatabases` also calls `orphanedBackups`, which `ls`'s `instanceBackupDir` (via the stdout-returning `execInPodOutput`, alongside the stdout-discarding `execInPod` used everywhere else) and reverses `backupFileName` on every `.fbk` it finds to report any whose database isn't in the current `spec.databases` — e.g. because the `Instance` was recreated with a different database list, or a database was dropped from `spec.databases` after a backup of it was taken. It explicitly skips the security database's own backup (`securityDatabaseBackupPath`), which sits in that same directory with the same `.fbk` naming but, by design, never has a matching `spec.databases` entry to reverse-match against — without that exclusion it would always show up as orphaned. Any remaining names are joined into `status.warning`, set (or cleared, when none are found) in the same `Status().Update` call as `status.databases`/`status.databaseCount`; the check is skipped entirely when nothing is pending, so a steady-state reconcile never pays for the extra `exec`, at the cost of `status.warning` only refreshing the next time `spec.databases` has something to create.

#### 6. Status fields

`status.databaseCount` is set to `len(status.databases)` in the same `Status().Update` call at the end of `reconcileDatabases` — kept as its own field, rather than computed on the fly, because CRD printer column `JSONPath`s can't apply a length function to an array. `status.phase` tracks the Instance's lifecycle: `Provisioning` until the StatefulSet pod is ready and every entry in `spec.databases` has been created, then `Ready` (set via the shared `setPhase` helper, which no-ops when the phase hasn't changed). `status.error` (the last reconcile failure, cleared on the next successful reconcile) and `status.message` (a human-readable summary: the error when one is set, otherwise `status.warning` when `orphanedBackups` has set one, otherwise the `Available` condition's message) are both maintained together in `setError`, which runs at the end of every `Reconcile`; `status.error` stays a clean success/failure signal for automation while `status.message` is what `kubectl get instances` shows, alongside `spec.version`, `status.phase`, and `status.databaseCount`, as printer columns (`api/v1/instance_types.go`'s `+kubebuilder:printcolumn` markers — regenerate `config/crd/bases/*.yaml` via `make manifests` after touching them).

#### 7. Finalizer and deletion

A `kubebird.github.io/finalizer` finalizer lets Reconcile observe deletion, log it, and report `status.phase: Deleting` (also via `setPhase`) before removing the finalizer so Kubernetes' garbage collection of the owner-referenced objects (ConfigMap, Service, StatefulSet) proceeds. `setDeletionMessage` (skip-if-unchanged like `setPhase`, but deliberately never touching `status.error` since deletion isn't a reconcile failure) records what `reconcileDeletion` is currently doing into `status.message` at each step — "Deleting Instance" up front, "Removing finalizer" right before that final step, and, when a backup volume exists (`spec.backup.enabled` is true and `spec.backup.type` has a `local` entry — see `backupVolumeSpec`), the `backupDatabases` sub-steps plus `releasePrimaryAndShadowStorage`'s own step below — so `kubectl get instances` reflects live deletion progress rather than a stale pre-deletion message. The primary/backup/shadow PVCs and the SYSDBA Secret are *not* owner-referenced — `reconcilePVC` and `reconcileSysdbaSecret` deliberately skip `controllerutil.SetControllerReference`, unlike every other resource Kubebird creates — so they're never part of that GC. That only actually matters for the backup PVC and the Secret, though: the primary and shadow PVCs are instead explicitly deleted by `releasePrimaryAndShadowStorage` as an unconditional step of `reconcileDeletion` (see below) regardless of `spec.backup`. So only the SYSDBA Secret, and the backup PVC whenever it exists, survive deleting the `Instance` unless someone removes them directly — an `Instance`'s primary/shadow data does not, unless a backup volume was configured to preserve it first.

#### 8. Backup-and-release on deletion

`reconcileDeletion` always calls `releasePrimaryAndShadowStorage` before removing the finalizer, which explicitly `r.Delete`s the primary PVC and, if `spec.storage.shadow` is set, the shadow PVC (the RBAC marker on `persistentvolumeclaims` includes `delete` for this) — the backup PVC itself is never touched by this step. When a backup volume exists (`backupVolumeSpec(instance) != nil`), `reconcileDeletion` first calls `backupDatabases`, which runs a final `gbak` backup of everything before that release happens; the backup PVC is then always left alone — there's no way to opt out of retaining it once it exists — so an `Instance` recreated under the same name afterward restores from it instead of starting fresh. Without a backup volume, `backupDatabases` is skipped entirely and there's no backup PVC to release either; the primary/shadow data (including the security database) is simply gone, and a recreated `Instance` starts completely fresh.

`gbak` can't back up a database — including, but not limited to, the security database — while a live server still has it open: confirmed against the actual image, this fails outright with "Database already opened with engine instance, incompatible with current" over a local connection, and "no permission for remote access to database" even via the services manager (`-se`), at least for the security database. So `backupDatabases` scales the StatefulSet to 0 replicas first, stopping the pod and releasing every database's live engine instance (not just the security database's), waits for it to actually terminate, then creates a short-lived `<name>-database-backup` Pod (`createDatabaseBackupPod`) that mounts the primary and backup PVCs (plus the shadow PVC too, when `spec.storage.shadow` is set — `gbak -backup` on a `shadow: true` database needs its shadow file to exist even though it doesn't back it up separately; confirmed against the actual image via "No such file or directory" without that mount) and runs `databaseBackupScript`: a local (no host, so no server involved at all) `gbak -backup -verify` for every entry in `status.databases`, writing `<database-without-.fdb>.fbk` into `instanceBackupDir` (`<backup mount>/base` — not named after the `Instance`, since the backup volume is already a PVC dedicated to it), then the same for the security database as `securityN.fbk`. None of it needs `-user`/`-password`: confirmed against the actual image, a local `gbak` backup of an already-existing database (security database included), run as root, doesn't validate them against anything. `backupDatabasesOffline` polls that Pod (`Get`, since nothing here needs `List`/`Watch` for its own logic — see the RBAC note below), deletes it once it reaches `Succeeded`, and deletes-and-retries it if it reaches `Failed` instead of getting permanently stuck re-observing the same failure forever.

- Every stage of `backupDatabases` is driven by observable cluster state rather than a status field, so it's naturally idempotent across the repeated reconciles the StatefulSet scale-down and pod termination both need: `spec.Replicas` still non-zero means the pod hasn't been asked to stop yet, `spec.Replicas` zero but `status.Replicas` still non-zero means it hasn't fully stopped yet, and both zero means it's safe to back up.
- Unlike `spec.databases`, the security database always exists by this point regardless of `spec.databases` — the security-database-init initContainer seeds one even for an Instance with none — so whenever this flow runs (i.e. a backup volume exists), it always backs up the security database too, rather than only when `status.databases` is non-empty.
- Each stage calls `setDeletionMessage`: `"Stopping the Firebird pod to back up its databases"` while scaling down and waiting for the pod to stop; `"Backing up databases into the backup volume"` while the helper Pod runs; then, regardless of whether the backup step ran at all, `"Releasing primary and shadow storage"` right before `releasePrimaryAndShadowStorage`'s `r.Delete`s.
- The `pods` RBAC marker (`instance_controller.go`) grants `list`/`watch` too, even though `createDatabaseBackupPod`/`backupDatabasesOffline` only ever call `Get`/`Create`/`Delete` on Pods directly: the manager's default client is a cached/informer-backed client, and the first `Get` or `Create` against a GVK it hasn't seen before starts a `List`+`Watch` informer for that whole type to populate its local cache — found the hard way, via `"pods is forbidden"` on `List` in the manager's own logs even with `get;create;delete` already granted.
- On a later recreate under the same name, when a backup volume was configured on the deleted `Instance`, this backed-up security database is what `security-database-init` restores (see "Core resources" above) instead of falling back to the image's stock default — the counterpart to `restoreDatabaseIfBackedUp` for `spec.databases`, but via a local `gbak -create` run directly by the init container rather than `reconcileDatabases`' `exec`ed one, since no firebird server is listening yet at that point for `exec` to reach. Without a backup volume, there is no backup to restore from either, so the recreated `Instance` gets a fresh security database (and fresh `spec.databases`) from the image's stock defaults just like a brand-new one.

### e2e coverage

Split across `test/e2e/instance_lifecycle_test.go`, `instance_backup_orphan_test.go`, `instance_table_restore_test.go`, `instance_version_upgrade_test.go`, and `instance_security_database_test.go`, each wired into the `Describe("Manager", Ordered, ...)` block in `test/e2e/e2e_test.go` via its own `instance*Specs()` function (`instanceLifecycleSpecs()`, `instanceBackupOrphanSpecs()`, `instanceTableRestoreSpecs()`, `instanceVersionUpgradeSpecs()`, `instanceSecurityDatabaseSpecs()`), all called after CRDs/manager are deployed and before they're torn down. There is no dedicated coverage for a no-`spec.backup` deletion against a real cluster — `internal/controller/instance_controller_test.go`'s envtest suite covers that path (see below) — since every scenario worth exercising against a real Kind cluster here needs a backup volume to have anything left to assert after deletion.

- **`instanceLifecycleSpecs`** exercises the `Instance` CRD end to end against a real Kind cluster: CRD cross-field validation (rejects `shadow: true` without `storage.shadow`); full deployment (Secret/ConfigMap/Service/StatefulSet, the primary/backup/shadow PVCs including the backup volume's mount, plus real database and shadow files created inside the pod via `exec`, not just checked via status); adding a database to a running `Instance` without disturbing existing ones or restarting the pod; a rotated SYSDBA password instead restarting the pod (its start time changes) and authenticating with the new password once it's back up; and garbage collection on deletion — owned objects (ConfigMap, Service, StatefulSet) removed, the backup PVC always retained (never owner-referenced), the SYSDBA Secret also retained (never owner-referenced) rather than garbage collected, and the primary/shadow PVCs released (`releasePrimaryAndShadowStorage` runs unconditionally on every deletion — see "Backup-and-release on deletion" above) after `backupDatabases` backs them up first since a local backup volume is configured here, with a throwaway verification Pod confirming `gbak` actually wrote a `.fbk` file per database, plus a `gbak`-backed-up `securityN.fbk` for the security database itself, into the backup volume's fixed `base/` subdirectory before they're released. As one further `It` in the same Ordered `Instance` Context, right after that deletion, it also covers restoring both databases (plus the shadow file) from those `.fbk` backups, and the security database from its own backup via the `security-database-init` initContainer, when the identical CR is re-applied under the same name, asserting no reconcile error and the actual files present in the pod — plus, since the surviving Secret is reused as-is across the delete/recreate (rather than being regenerated), that the password read afterwards is unchanged from the one captured just before deletion and that SYSDBA actually authenticates against the restored database with it.
- **`instanceBackupOrphanSpecs`** exercises deleting and recreating an `Instance` under the same name with a changed `spec.databases`: a database dropped from the new generation is never restored and its backup file is left orphaned (surfaced via `status.warning`), a database kept in both generations is restored from its backup, and a database newly added has no backup to restore from and is created fresh.
- **`instanceTableRestoreSpecs`** exercises that user data, not just the database file's existence, survives repeated delete/recreate cycles: a table created after first provisioning is still there after one backup/restore round, and a second table created after that restore joins the first after a second round.
- **`instanceVersionUpgradeSpecs`** exercises deleting and recreating an `Instance` under the same name with `spec.version` bumped to a new Firebird major version (`3.0.14` → `4.0.3`): a marker table created before deletion is still present after the recreated `Instance` restores its backup, the recreated StatefulSet's pod runs the new image tag, `status.error` stays empty, and SYSDBA authenticates against the upgraded server with the SYSDBA Secret's password (the Secret is never owner-referenced, so it survives the delete/recreate and keeps the same password throughout) — proving the backup/restore path (and the security database relocation feeding into it) tolerates a major-version bump across the recreate.
- **`instanceSecurityDatabaseSpecs`** exercises the `security-database-init` init container's backup/restore path for the security database's own content specifically, as opposed to just the SYSDBA password: with a local backup volume configured, a Firebird user created directly in the security database (not the SYSDBA account synced from the Secret) survives deleting and recreating the `Instance` under the same name, restored via `gbak` from the backup `backupDatabases` took before the primary PVC was released — proving the backup/restore round-trip carries over arbitrary security database content, not just the well-known SYSDBA row the image's own entrypoint re-applies from the Secret on every start regardless (which is all `instanceLifecycleSpecs` above checks for the security database). Also confirms SYSDBA can still authenticate with the (unowned, and so unchanged) Secret's password against the restored security database, and that `status.error` stays empty across the recreate.

`internal/controller/instance_controller_test.go`'s envtest suite (unit-level, no real Kind cluster) covers all three deletion paths directly against the controller: a `Context("When deleting an Instance with a local backup volume configured", ...)` exercising the stop-pod/backup/release sequence step by step and asserting the backup PVC is always kept while the primary PVC is released; a `Context("When deleting an Instance without backup enabled", ...)` asserting the primary PVC is still released in a single reconcile, with no pod-stop/backup dance and no backup PVC to consider at all; and a `Context("When backup is enabled but spec.backup.type has no local entry", ...)` asserting no backup PVC is ever created and deletion still completes in a single reconcile, guarding against `reconcileDeletion` regressing back to gating on the raw (and here misleading) `spec.backup.enabled` field instead of `backupVolumeSpec`.
