# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project overview

Kubebird is a Kubernetes operator, scaffolded with Kubebuilder v4 (go.kubebuilder.io/v4, single-group layout), that installs and manages [Firebird RDBMS](https://firebirdsql.org/) instances via a namespaced `Instance` custom resource in API group `kubebird.github.io/v1`.

The project is in an early, mostly-scaffolded state: `InstanceSpec` (`api/v1/instance_types.go`) currently only has the generated placeholder `Foo` field, and `Reconcile()` (`internal/controller/instance_controller.go`) is a no-op stub. The target shape of the CR — the part not yet implemented — is documented in README.md:

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
  authentication:
    sysdba:
      secretRef: ""
```

When implementing this, treat the README sample as the source of truth for spec shape, and check with the user before diverging from it.

## Commands

```bash
make manifests generate   # after editing api/v1/*_types.go: regen CRDs/RBAC + DeepCopy methods
make fmt vet               # format and vet
make lint                  # golangci-lint (config: .golangci.yml)
make lint-fix               # golangci-lint --fix
make test                  # unit/envtest suite (runs manifests generate fmt vet setup-envtest first)
make test-e2e               # e2e suite against a Kind cluster (creates/tears down $KIND_CLUSTER, default "kubebuilder-test-e2e")
make run                   # run the manager locally against the current kubeconfig context
```

Run a single test (Ginkgo/Gomega BDD style, under envtest):

```bash
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" go test ./internal/controller/... -run TestControllers -coverprofile cover.out
# or scope to one spec via Ginkgo focus, e.g. add an `F` prefix (FIt/FDescribe) or:
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" go test ./internal/controller/... --ginkgo.focus="should successfully reconcile"
```

`make test` excludes `./test/e2e/...` (`go list ./... | grep -v /e2e`). e2e tests are build-tagged `e2e` and require a Kind cluster — never point `test-e2e` at a real dev/prod cluster.

## Architecture

Standard Kubebuilder single-group layout — see `AGENTS.md` in this repo for the full scaffolding reference (file layout, marker conventions, RBAC markers, CLI commands for adding APIs/webhooks). Key points specific to this repo:

- **API group wiring**: `PROJECT` declares `group: kubebird`, `domain: github.io`; these compose to `kubebird.github.io`, which must stay consistent across `api/v1/groupversion_info.go` (`+groupName`), the RBAC markers in `internal/controller/instance_controller.go`, and any sample/manifest YAML. A mismatch here (e.g. a doubled `kubebird.github.io.github.io`) breaks the CRD/manifest wiring silently.
- **Generated vs. owned files**: `api/v1/zz_generated.deepcopy.go`, `config/crd/bases/*.yaml`, and `config/rbac/role.yaml` are produced by `make manifests generate` — never hand-edit them. `api/v1/instance_types.go` and `internal/controller/instance_controller.go` are the owned files to extend.
- **envtest CRD source**: `internal/controller/suite_test.go` loads CRDs from `config/crd/bases/` for the test API server (`ErrorIfCRDPathMissing: true`), so `make manifests` must be run (and the CRD committed/present) before `make test` will pass.
- **Namespace scoping**: `cmd/main.go` requires a `WATCH_NAMESPACE` env var and configures the manager's cache to watch only that namespace (or comma-separated list) via `setupCacheNamespaces` — the manager will not start without it.
- **Logging convention**: this repo follows the Kubernetes logging style guide (capitalized message, no trailing period, active/past voice, object type named explicitly) — enforced in part by the custom `logcheck` golangci-lint module in `.golangci.yml`.
