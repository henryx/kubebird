# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project overview

Kubebird is a Kubernetes operator, built in Python on top of `kopf`, that installs and manages
[Firebird RDBMS](https://firebirdsql.org/) instances via a namespaced `Instance` custom resource.

The project is at an early stage. `kopf` and `kubernetes` (the official Python client) are declared
dependencies, and `src/kubebird/create.py` holds the first handler, `create_fn` (`@kopf.on.create(
kind="Instance", version="v1", group="kubebird.github.io")`). Its body is currently a no-op
(`pass`), so no reconciliation logic exists yet — treat the README's architecture description as
the design target, not as a description of existing code.

There is currently no CLI entry point: `pyproject.toml` has no `[project.scripts]` section, and
`src/kubebird/__init__.py` only declares `__all__ = ["create"]` (no `main()`). Until an entry point
is added, run the operator directly via `kopf`, e.g. `uv run kopf run -m kubebird.create`.

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

`tests/conftest.py` also defines a function-scoped `kubeconfig` fixture, built on top of `k3s`,
that writes the container's kubeconfig to a temp file, sets `KUBECONFIG` to it, and yields the
path (deleting the file on teardown). `tests/test_create.py` uses it for a functional test per
kopf's [testing docs](https://docs.kopf.dev/en/stable/testing/): it applies `deploy/crd.yaml` and
`deploy/cr.yaml` via the `kubernetes` client library (a direct, non-dev dependency — used here as
the test client and, incidentally, by kopf itself as an optional auth piggyback, see below), runs
the operator in-process with `kopf.testing.KopfRunner(["run", ..., "-m", "kubebird.create"])`, and
asserts the `create_fn` handler logs success. This exercises `create_fn` for the first time, so
`src/kubebird` coverage is no longer 0%, even though the handler itself is still a no-op.

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

Reconciling this CR is expected to:

- Deploy the Firebird instance as a `StatefulSet` using the given `image`/`version`.
- Create a `Service` for the instance (default type `ClusterIP`).
- Create a `PVC` sized per `storage.size`, using the cluster default `StorageClass` when
  `storage.class` is empty.
- Instantiate the databases listed under `databases`, honoring `shadow` mode per database.
- Optionally manage authentication: if `authentication.sysdba.secretRef` is unset, the operator
  generates a `<instance-name>-sysdba` secret with a random password; if
  `authentication.user.secretRef` is unset, no non-SYSDBA user is created.

`tests/test_create.py` deploys a real `Instance` CR on top of the `k3s` fixture (see "Development
commands" above) and runs the operator against it, but since `create_fn` is a no-op, no actual
Firebird pod gets created yet — that end-to-end step (a real pod backing the `Instance`) is still
to come. See README.md for the manual `docker run` equivalent used to exercise a Firebird
container directly.

`deploy/crd.yaml` holds the `CustomResourceDefinition` (OpenAPI v3 schema for the `spec`/`status`
shape above) and `deploy/cr.yaml` holds a sample `Instance` matching it. Keep both files in sync
with each other and with the README's sample whenever the CR shape changes.

The README's Installation section documents `kubectl apply -f deploy/crd.yaml -f deploy/operator.yaml`,
but `deploy/operator.yaml` (the Deployment/RBAC manifest for running the operator itself in-cluster)
does not exist yet — only `crd.yaml` and `cr.yaml` are present under `deploy/`.

## Requirements

Python >= 3.14 (see `.python-version`, pinned to 3.14). `pyproject.toml` currently defines no
`[project.scripts]` entry point (see "Development commands" above for how to run the operator).
