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
make build-installer        # regen manifests + emit dist/install.yaml (CRD+RBAC+Deployment); IMG defaults to quay.io/kubebird/controller:latest
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

- **API group wiring**: `PROJECT` declares `group: kubebird`, `domain: github.io`; these compose to `kubebird.github.io`, which must stay consistent across `api/v1/groupversion_info.go` (`+groupName`), the RBAC markers in `internal/controller/instance_controller.go`, and any sample/manifest YAML. A mismatch here (e.g. a doubled `kubebird.github.io.github.io`) breaks the CRD/manifest wiring silently.
- **Generated vs. owned files**: `api/v1/zz_generated.deepcopy.go`, `config/crd/bases/*.yaml`, and `config/rbac/role.yaml` are produced by `make manifests generate` — never hand-edit them. `api/v1/instance_types.go` and `internal/controller/*.go` are the owned files to extend.
- **envtest CRD source**: `internal/controller/suite_test.go` loads CRDs from `config/crd/bases/` for the test API server (`ErrorIfCRDPathMissing: true`), so `make manifests` must be run (and the CRD committed/present) before `make test` will pass.
- **Namespace scoping**: `cmd/main.go` requires a `WATCH_NAMESPACE` env var and configures the manager's cache to watch only that namespace (or comma-separated list) via `setupCacheNamespaces` — the manager will not start without it. Don't confuse this with where the operator itself runs: `config/default/kustomization.yaml` deploys the manager, RBAC and Service into the `kubebird-system` namespace (via `namespace:`/`namePrefix: kubebird-`), which must stay paired — kustomize requires `namePrefix` to match the text before the first `-` in `namespace`. The `app.kubernetes.io/name: kubebird` label/selector pair (`config/manager/manager.yaml`, `config/default/metrics_service.yaml`, `config/network-policy/allow-metrics-traffic.yaml`, `config/prometheus/monitor.yaml`) must also change together — it's how the metrics Service, NetworkPolicy and ServiceMonitor find the manager Pod.
- **Release/publishing**: `.github/workflows/release.yml` triggers only on pushing a semver tag (`[0-9]+.[0-9]+.[0-9]+`, e.g. `0.2.0`) — it builds/pushes the manager image to `quay.io/kubebird/controller` (versioned + `latest`) and attaches `dist/install.yaml` to a GitHub Release. `Makefile`'s `IMG` default and `config/manager/kustomization.yaml`'s `images:` transform both point at that same registry, so `make build-installer`/`make docker-build docker-push` without an explicit `IMG=` also target `quay.io/kubebird/controller`.
- **Logging convention**: this repo follows the Kubernetes logging style guide (capitalized message, no trailing period, active/past voice, object type named explicitly) — enforced in part by the custom `logcheck` golangci-lint module in `.golangci.yml`.
- **Reconcile flow** (`internal/controller/instance_controller.go`, split across `instance_resources.go` and `instance_provision.go`): for each `Instance`, the controller (1) creates the SYSDBA `Secret` (name from `spec.authentication.sysdba.secretRef`, defaulting to `<name>-sysdba` when unset) with a generated random password and a `username: SYSDBA` key if it doesn't exist yet, never touching one that already exists; (2) reconciles a `ConfigMap` registering one Firebird alias per `spec.databases[]` entry (`alias` if set, else the database name) in a `databases.conf` mounted (via `SubPath`) at `/opt/firebird/databases.conf`, a `Service`, PVCs named `<name>-primary`/`<name>-shadow` from `spec.storage` (created by `reconcilePVC`/`reconcilePVCs` if missing — `get;list;watch;create` only, size/class are effectively immutable once created, same as the volumeClaimTemplates approach it replaced), and a single-replica `StatefulSet` running the `firebirdsql/firebird` image, which mounts those PVCs directly by claim name rather than via `spec.volumeClaimTemplates`. Because a `SubPath` mount replaces that file wholesale rather than merging with the image's own copy, `mutateAliasesConfigMap` also appends a `security.db` alias (`$(dir_secDb)/security<major>.fdb`, `RemoteAccess = false`) for the security database, so the image's default alias for it isn't silently dropped; (3) once the StatefulSet pod is ready, pushes the Secret's password to the live server whenever it drifts from `status.sysdbaPasswordHash` by `exec`ing `isql` directly against the security database file (e.g. `security3.fdb` for a `3.x` version, via the shared `securityDatabaseFileName` helper also used for the `security.db` alias above) rather than over the usual host:port connection — that embedded/local connection is trusted by OS user rather than by password, which sidesteps needing the password being replaced to authenticate; (4) `exec`s `isql` inside the `firebird` container (via `client-go`'s `remotecommand`, requiring the `pods/exec` RBAC verb) to run `CREATE DATABASE`/`CREATE SHADOW` for each entry in `spec.databases` not yet recorded in `status.databases`, and `DROP DATABASE` (via the `databaseDropScript` constant, isql connecting by passing the database's path as its positional argument — Firebird drops any attached shadow along with it) for each entry in `status.databases` no longer present in `spec.databases`. Databases are provisioned this way — not via a mounted init script — specifically because the `nakagami/firebirdsql` Go driver's database-creation path hardcodes `page_size=4096` and always sets `isc_dpb_overwrite`, which would silently ignore `spec.databases[].pageSize` and risk clobbering an existing database file; `isql` over `exec` preserves full fidelity to the CRD fields. `status.databases` is what makes both directions idempotent — a database already listed there is never re-created, and one no longer listed is never re-dropped. `status.databaseCount` is set to `len(status.databases)` in the same `Status().Update` call at the end of `reconcileDatabases` — kept as its own field, rather than computed on the fly, because CRD printer column `JSONPath`s can't apply a length function to an array. Removing a `spec.databases` entry also drops its alias from the next `databases.conf` regeneration, since `mutateAliasesConfigMap` rebuilds that file from `spec.databases` alone on every reconcile. `status.phase` tracks the Instance's lifecycle: `Provisioning` until the StatefulSet pod is ready and every entry in `spec.databases` has been created, then `Ready` (set via the shared `setPhase` helper, which no-ops when the phase hasn't changed). `status.error` (the last reconcile failure, cleared on success) and `status.message` (a human-readable summary: the error when one is set, otherwise the `Available` condition's message) are both maintained together in `setError`, which runs at the end of every `Reconcile`; `status.error` stays a clean success/failure signal for automation while `status.message` is what `kubectl get instances` shows, alongside `spec.version`, `status.phase`, and `status.databaseCount`, as printer columns (`api/v1/instance_types.go`'s `+kubebuilder:printcolumn` markers — regenerate `config/crd/bases/*.yaml` via `make manifests` after touching them). A `kubebird.github.io/finalizer` finalizer lets Reconcile observe deletion, log it, and report `status.phase: Deleting` (also via `setPhase`) before removing the finalizer so Kubernetes' garbage collection of the owner-referenced objects (Secret, ConfigMap, Service, StatefulSet) proceeds. The primary/shadow PVCs are *not* owner-referenced — `reconcilePVC` deliberately skips `controllerutil.SetControllerReference` on them, unlike every other resource Kubebird creates — so they're never part of that GC, meaning an `Instance`'s data survives deleting the `Instance` unless someone removes the PVCs directly. Every object Kubebird creates is labelled `kubebird.github.io/instance: <name>`. Any error from a reconcile is recorded verbatim in `status.error` and cleared on the next successful reconcile.
- **e2e coverage** (`test/e2e/instance_test.go`, wired into the `Describe("Manager", Ordered, ...)` block in `test/e2e/e2e_test.go` via `instanceLifecycleSpecs()` so it runs after CRDs/manager are deployed and before they're torn down): exercises the `Instance` CRD end to end against a real Kind cluster — CRD cross-field validation (rejects `shadow: true` without `storage.shadow`), full deployment (Secret/ConfigMap/Service/StatefulSet plus real database and shadow files created inside the pod via `exec`, not just checked via status), adding a database to a running `Instance` without disturbing existing ones or restarting the pod, live SYSDBA password rotation (authenticates with the new password against the running server), and owner-reference garbage collection on deletion.
