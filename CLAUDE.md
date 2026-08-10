# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project overview

Kubebird is a Kubernetes operator, built in Python on top of `kopf`, that installs and manages
[Firebird RDBMS](https://firebirdsql.org/) instances via a namespaced `Instance` custom resource.

The project is at an early stage but `create_fn` (`src/kubebird/create.py`, `@kopf.on.create(
kind="Instance", version="v1", group="kubebird.github.io")`) now implements the full reconciliation
described in "Architecture" below. `kopf` and `kubernetes` (the official Python client) are
declared dependencies; there is no `on.update`/`on.delete` handler yet (deletion relies entirely on
Kubernetes garbage-collecting the owned objects via `kopf.adopt()`).

The implementation is split across three modules:
- `src/kubebird/k8s.py` — builds the `Secret`/`PersistentVolumeClaim`/`Service`/`StatefulSet`
  manifests (plain dicts, not typed client models) and idempotent creation helpers
  (`create_or_ignore` treats a 409 Conflict as "already created by a previous handler retry").
- `src/kubebird/firebird.py` — provisions the instance over `kubectl exec`-style pod exec, using
  `isql` piped through `/bin/sh -c`: waits for the pod to be `Ready`, then separately waits for
  SYSDBA authentication to actually work (see gotcha below), then runs `CREATE DATABASE`/`CREATE
  SHADOW`/`CREATE USER`.
- `src/kubebird/create.py` — the `@kopf.on.create` handler that orchestrates the two modules above
  and reports progress via `status.phase` (`Provisioning` → `WaitingForPod` →
  `ProvisioningDatabases` → `Ready`).

There is currently no CLI entry point: `pyproject.toml` has no `[project.scripts]` section, and
`src/kubebird/__init__.py` only declares `__all__ = ["create", "firebird", "k8s"]` (no `main()`).
Until an entry point is added, run the operator directly via `kopf`, e.g.
`uv run kopf run -m kubebird.create`.

Gotchas hit while building `firebird.py` (all fixed, but easy to reintroduce):
- A bare local path (e.g. `/var/lib/firebird/data/x.fdb`) makes `isql` connect through Firebird's
  embedded/local provider instead of the already-running SuperServer, which races it for an
  exclusive lock on the security database and breaks `CREATE USER` with a lock error. Every `isql`
  connection target must be prefixed `localhost:`.
- The exec websocket's exit-code channel is unreliable across negotiated subprotocol versions
  (`channel.k8s.io` vs `v4`/`v5`); `run_isql` instead checks the captured output text for
  Firebird's own `"Statement failed"` marker.
- The image applies `FIREBIRD_ROOT_PASSWORD` *after* the container is marked `Ready` (no readiness
  probe distinguishes this), so `create_fn` must additionally wait for SYSDBA authentication to
  actually be live (`firebird.wait_for_sysdba_ready`, a throwaway `CREATE DATABASE`/`DROP DATABASE`
  probe) before issuing any real SQL.
- `k8s.ensure_sysdba_secret` must be idempotent across kopf handler retries: it generates a random
  password, but if the secret already exists (409, from a prior attempt) it must re-read that
  secret's *actual* stored password rather than using the freshly generated one that was never
  written anywhere — otherwise a retry uses a password that no longer matches the live server.

## Development commands

This project uses `uv` for dependency management and `tox` for running tasks in isolated envs.

```bash
# Sync dependencies (dev group included)
uv sync


# Run tests directly (no coverage)
uv run pytest tests

# Run a single test
uv run pytest tests/path/to/test_file.py::test_name

# Lint
uv run ruff check

# Format
uv run ruff format
```

Equivalent tox environments (each runs in its own venv via `uv`):

```bash
tox -e lint      # ruff check
tox -e format    # ruff format
tox -e py3       # uv sync + pytest with coverage (--cov=src/kubebird)
tox -e report    # coverage report + html
tox -e clean     # coverage erase
```

Run the full default suite (`clean, lint, format, py3, report`) with plain `tox`. There is a
`type` env defined in `tox.ini` for `mypy` (targets `src/kubebird tests`), but it is currently
commented out of `env_list` — add it back there to enable it.

`tests/conftest.py` defines a session-scoped `k3s` fixture (via `testcontainers`'s
`K3SContainer`, from `testcontainers.community.k3s` — `testcontainers.k3s` is deprecated) that
starts a real k3s container per test session. `testcontainers` is a dev dependency for this.
`tests/test_k3s.py` only exercises the fixture itself (asserts `k3s.config_yaml()` returns a
valid kubeconfig).

Gotcha: on cgroup v2 hosts using the systemd cgroup driver, Docker gives the k3s container its own
private cgroup namespace, which its embedded kubelet can't reconcile with the host cgroup paths
bind-mounted in — *every* pod (even built-in ones like coredns) then stays `Pending` forever with
`FailedCreatePodSandBox: ... cgroup.procs: no such file or directory`. The `k3s` fixture works
around this by forcing `container._kwargs["cgroupns"] = "host"` before starting it (a private
attribute — `K3SContainer` has no public API for extra `docker run` kwargs).

`tests/conftest.py` also defines a function-scoped `kubeconfig` fixture, built on top of `k3s`,
that writes the container's kubeconfig to a temp file, sets `KUBECONFIG` to it, and yields the
path (deleting the file on teardown). `tests/test_create.py` uses it for a functional test per
kopf's [testing docs](https://docs.kopf.dev/en/stable/testing/): it applies `deploy/crd.yaml` and
`deploy/cr.yaml` via the `kubernetes` client library (a direct, non-dev dependency — used here as
the test client and, incidentally, by kopf itself as an optional auth piggyback, see below), runs
the operator in-process with `kopf.testing.KopfRunner(["run", ..., "-m", "kubebird.create"])`,
waits for `status.phase` to reach `Ready` (up to 420s — provisioning involves an image pull, a real
Firebird startup, and one expected retry while waiting for SYSDBA auth to become live), and execs
into the pod to confirm the database file actually exists on disk before deleting the CR. This is a
real, non-mocked run — it takes ~80s end-to-end on a warm image cache.

Gotcha: as soon as the `kubernetes` package is importable, `kopf` prefers piggybacking on it for
authentication (`kopf._core.intents.piggybacking.login_via_client`) over its own lightweight
kubeconfig parsing. `kubernetes.config.kube_config` bakes `KUBECONFIG` into a module-level
constant (`KUBE_CONFIG_DEFAULT_LOCATION`) the first time it is imported — which happens as soon as
`kopf` itself is imported (pytest imports test modules, hence `kopf`, at collection time, before
any fixture runs). Setting the `KUBECONFIG` env var alone is therefore too late; the `kubeconfig`
fixture also monkeypatches that constant directly so kopf's client-based login connects to the
`k3s` container instead of the real `~/.kube/config`.

## Architecture

The operator centers on a single CRD, `Instance` (`kubebird.github.io/v1`), namespaced. One CR
represents one Firebird instance. Example spec (see README.md for the full annotated version):

```yaml
apiVersion: kubebird.github.io/v1
kind: Instance
metadata:
  name: test
  namespace: default
spec:
  image: firebirdsql/firebird
  version: 3.0.14
  databases:
    - name: "instance.fdb"
      shadow: false
  service:
    type: ClusterIP
  storage:
    class: ""
    size: 3Gi
  authentication:
    sysdba:
      secretRef: ""
    user:
      secretRef: ""
```

Reconciling this CR (`create_fn` in `src/kubebird/create.py`):

- Deploys the Firebird instance as a `StatefulSet` (1 replica) using the given `image`/`version`,
  mounting the PVC below at `k8s.DATA_MOUNT_PATH` (`/var/lib/firebird/data`).
- Creates a `Service` for the instance (type from `spec.service.type`, default `ClusterIP`),
  targeting port 3050.
- Creates a `PVC` sized per `storage.size`, using the cluster default `StorageClass` when
  `storage.class` is empty (a plain PVC referenced by the pod's `volumes`, not a
  `volumeClaimTemplate`, since the CR describes exactly one instance/pod).
- Instantiates every entry in `databases` via `isql` exec'd into the pod, creating a `CREATE
  SHADOW 1` file (`<path>.shadow`) for any entry with `shadow: true`.
- Manages authentication: if `authentication.sysdba.secretRef` is unset, generates a
  `<instance-name>-sysdba` secret (keys `username: SYSDBA`, `password: <random>`) and wires it
  into the `StatefulSet` via `FIREBIRD_ROOT_PASSWORD`/`secretKeyRef`; if set, reads that secret's
  `password` key instead. If `authentication.user.secretRef` is set, reads its `username`/
  `password` keys and issues `CREATE USER` for it; if unset, no non-SYSDBA user is created. These
  `username`/`password` key names are a kubebird convention, not documented anywhere else — keep
  README.md's authentication bullets in sync if this ever changes.
- Adopts every created object with `kopf.adopt()`, so deleting the `Instance` garbage-collects them
  via owner references — there is no `@kopf.on.delete` handler (or `@kopf.on.update`) yet.

`tests/test_create.py` deploys a real `Instance` CR on top of the `k3s` fixture (see "Development
commands" above), runs `create_fn` against it, and confirms the database file actually exists in
the pod — a genuine end-to-end run, not a mock.

`deploy/crd.yaml` holds the `CustomResourceDefinition` (OpenAPI v3 schema for the `spec`/`status`
shape above) and `deploy/cr.yaml` holds a sample `Instance` matching it. Keep both files in sync
with each other and with the README's sample whenever the CR shape changes.

The README's Installation section documents `kubectl apply -f deploy/crd.yaml -f deploy/operator.yaml`,
but `deploy/operator.yaml` (the Deployment/RBAC manifest for running the operator itself in-cluster)
does not exist yet — only `crd.yaml` and `cr.yaml` are present under `deploy/`. Now that `create_fn`
is implemented, that RBAC needs to grant (at least): `get`/`list`/`watch`/`patch` on `instances`
(and their `status` subresource) for the CRD's group; `create` on `secrets`,
`persistentvolumeclaims`, `services`, `statefulsets`; `get` on `secrets`; `get` on `pods` and
`create` on `pods/exec` (needed for the `isql` provisioning in `firebird.py`); plus whatever kopf
itself needs for peering/events (see kopf's own RBAC docs).

## Requirements

Python >= 3.14 (see `.python-version`, pinned to 3.14). `pyproject.toml` currently defines no
`[project.scripts]` entry point (see "Development commands" above for how to run the operator).
