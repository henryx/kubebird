# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project overview

Kubebird is a Kubernetes operator, built in Python on top of `kopf`, that installs and manages
[Firebird RDBMS](https://firebirdsql.org/) instances via a namespaced `Instance` custom resource.

The project is at an early, pre-implementation stage: `src/kubebird/__init__.py` currently only
contains a `main()` placeholder, and the `kopf` dependency described below has not yet been added
to `pyproject.toml`. Treat the README's architecture description as the design target, not as a
description of existing code.

## Development commands

This project uses `uv` for dependency management and `tox` for running tasks in isolated envs.

```bash
# Sync dependencies (dev group included)
uv sync

# Run the CLI entry point
uv run kubebird

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
`type` env defined in `tox.ini` for `mypy`, but it is currently commented out of `env_list` and
also references a stale path (`ssot_web`) — fix that path before re-enabling it.

There is no `tests/` directory yet; add one when writing the first tests, matching the
`--cov=src/kubebird` target already configured in `tox.ini`.

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

End-to-end tests are intended to use `testcontainers` with a `k3s` image, deploying a real
`Instance` backed by a single pod (see README.md for the manual `docker run` equivalent used to
exercise a Firebird container directly).

## Requirements

Python >= 3.14 (see `.python-version`, pinned to 3.14). The `kubebird` console script is defined
in `pyproject.toml` and points at `kubebird:main`.
