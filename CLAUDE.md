# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project overview

Kubebird is a Kubernetes operator, built in Python on top of `kopf`, that installs and manages
[Firebird RDBMS](https://firebirdsql.org/) instances via a namespaced `Instance` custom resource.

Kubebird implements the full `Instance` lifecycle described in "Architecture" below: `create_fn`
(`src/kubebird/create.py`, `@kopf.on.create(kind="Instance", version="v1",
group="kubebird.github.io")`) provisions it, `update_fn`/`sysdba_secret_update_fn`
(`src/kubebird/update.py`) reconcile spec changes and SYSDBA secret rotations, and `delete_fn`
(`src/kubebird/delete.py`) handles deletion. `kopf` and `kubernetes` (the official Python client)
are declared dependencies. `deploy/operator.yaml` runs the operator in-cluster (Deployment + RBAC,
see "Architecture" below); still missing: `update_fn` support for changes to
`authentication`/`storage` (only `service`/`version`/`databases` are reconciled today).

The implementation is split across five modules:
- `src/kubebird/k8s.py` — builds the `Secret`/`PersistentVolumeClaim`/`Service`/`StatefulSet`
  manifests (plain dicts, not typed client models) and idempotent creation helpers
  (`create_or_ignore` treats a 409 Conflict as "already created by a previous handler retry").
- `src/kubebird/firebird.py` — provisions the instance over `kubectl exec`-style pod exec, using
  `isql` piped through `/bin/sh -c`: waits for the pod to be `Ready`, then separately waits for
  SYSDBA authentication to actually work (see gotcha below), then runs `CREATE DATABASE`/`CREATE
  SHADOW`. Also runs `gsec` the same way to change the live SYSDBA password
  (`change_sysdba_password`).
- `src/kubebird/create.py` — the `@kopf.on.create` handler that orchestrates the two modules above
  and reports progress via `status.phase` (`Provisioning` → `WaitingForPod` →
  `ProvisioningDatabases` → `Ready`); its actual reconciliation work lives in a private
  `_reconcile()` so `create_fn` itself can wrap the call in `try`/`except Exception`, recording
  `str(exc)` into `status.error` (cleared to `""` on success) before re-raising — this is what
  surfaces a handler failure (RBAC errors, `kopf.PermanentError`/`TemporaryError`, anything) into
  `kubectl get instances` via the CRD's `Error` printer column, not just into the operator's own
  pod logs. Re-raising after recording it is what keeps kopf's own retry/backoff behavior intact —
  this only adds visibility, it doesn't change control flow.
- `src/kubebird/update.py` — `update_fn` (`@kopf.on.update` on `Instance`) reconciles `Service`
  type and `StatefulSet` image/version changes and provisions any databases newly added to
  `spec.databases`; `sysdba_secret_update_fn` (`@kopf.on.update` on core `Secret`, filtered by the
  `kubebird.github.io/role: sysdba` label) pushes a rotated SYSDBA secret password to the live
  server via `gsec`. `update_fn` follows the same `_reconcile()` + `try`/`except` →
  `status.error` pattern as `create_fn` (see below); `sysdba_secret_update_fn` doesn't, since it
  reconciles a `Secret`, not an `Instance` — there's no `Instance.status` for it to write into.
- `src/kubebird/delete.py` — `delete_fn` (`@kopf.on.delete` on `Instance`). Deliberately minimal:
  it just logs and patches `status.phase`, since every object `create_fn`/`update_fn` create is
  already `kopf.adopt()`-ed, so Kubernetes garbage-collects all of them (Secret, PVC(s), Service,
  StatefulSet) through their owner references as soon as this handler returns. Its only real effect
  is the finalizer kopf attaches for having *any* `on.delete` handler registered at all, which
  blocks the `Instance`'s own removal until the handler completes.

`src/kubebird/operator.py` is the CLI entry point (`pyproject.toml`'s `[project.scripts]` maps
`kubebird-operator` to `kubebird.operator:main`). It imports `kubebird.create`/`delete`/`update`
purely for their `@kopf.on.*` decorators' side effect of registering handlers in kopf's default
registry — required
because, unlike the CLI's `-m` flag, `kopf.run()` only sees handlers from modules already imported
by the time it's called. It also runs the operator on `uvloop`: kopf's own CLI auto-detects and
injects uvloop, but only for the CLI (`kopf._kits.loops.proper_loop`) — `kopf.run()` itself does
not manage the event loop when embedded like this, so `main()` replicates that CLI-internal
mechanism explicitly (`asyncio.Runner(loop_factory=uvloop.new_event_loop)`, then
`kopf.run(loop=runner.get_loop())`). It also reads a `NAMESPACE` env var and, if set, passes it as
`kopf.run(namespaces=[NAMESPACE])` to scope the operator to one namespace (e.g. via the pod's
Downward API); left unset, `kopf.run()` falls back to its own default (cluster-wide/current
context), matching `kopf run` with neither `-n` nor `-A`. It also reads a `LOG_LEVEL` env var
(`DEBUG`/`INFO`/`WARNING`/`ERROR`/`CRITICAL`, defaulting to and falling back to `INFO` on an unset
or unrecognized value) to control verbosity: `main()` calls `kopf.configure(debug=(log_level ==
"DEBUG"), verbose=(log_level == "DEBUG"), quiet=(log_level == "WARNING"))` first — kopf's
documented hook for controlling logging when embedding `kopf.run()` directly, as opposed to the
`kopf run` CLI's own `--verbose`/`--debug`/`--quiet` flags — then explicitly overrides the root
logger's level via `logging.getLogger().setLevel(log_level)` right after. Of `configure()`'s three
knobs, only `debug` has any effect actually visible here: it's the one that decides whether kopf's
own internal loggers (e.g. `asyncio`) propagate instead of being muted, and it's the only thing our
subsequent explicit `setLevel()` call doesn't also override — `configure()`'s own `log_level`
computation from `debug`/`verbose`/`quiet` (which only distinguishes `DEBUG`/`INFO`/`WARNING`, with
no separate `ERROR`/`CRITICAL`) gets discarded the moment `setLevel(log_level)` runs afterward, so
passing `verbose`/`quiet` through as well is harmless but redundant. Verified directly against kopf
1.44.6's source (`kopf._core.actions.loggers.configure`) and empirically for every `LOG_LEVEL`
value: none of kopf's own loggers (`kopf.objects`, the one `create_fn`/`update_fn`/`delete_fn`'s
`logger` parameter is backed by, included) set an explicit level anywhere in the package, so they
all inherit whatever effective level the root logger ends up at. `deploy/operator.yaml`'s
Deployment sets a fixed `LOG_LEVEL: INFO` alongside `NAMESPACE`. It is a regular (non-executable)
file
with no `#!/usr/bin/env python3` shebang and no `if __name__ == "__main__": main()` guard, so it
is *not* directly executable (`python -m kubebird.operator` would import it and define `main()`
without calling it); the `kubebird-operator` console script (which calls `main()` itself via its
generated wrapper) is the only way to run it.

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
- `firebird.wait_for_pod_ready` must call `core_api.read_namespaced_pod`, not
  `read_namespaced_pod_status`: the latter hits the `pods/status` *subresource* endpoint
  (`GET .../pods/{name}/status`), which RBAC treats as a distinct resource from plain `pods` and so
  needs its own grant — `deploy/operator.yaml`'s Role only ever granted `get` on `pods` itself. Hit
  this for real against a live cluster: `create_fn` got stuck retrying every 60s on a `403` ("cannot
  get resource \"pods/status\""). The main resource's GET already returns the identical
  `.status.conditions`, so switching to it fixed the 403 without needing to widen the
  ServiceAccount's Role at all.
- That same function must also tolerate a `404` from `read_namespaced_pod`, not just poll on
  conditions: right after `create_fn`/`update_fn` creates the `StatefulSet`, there's a real window
  where the StatefulSet controller hasn't created the pod object yet at all, and a GET for it 404s
  outright rather than returning an object with no `Ready` condition. Before the `status.error`
  feature below existed, this only ever showed up as a transient `ERROR`-logged exception that kopf
  silently retried past (so it went unnoticed); once `status.error` started surfacing every handler
  exception, this exact 404 showed up as a false-positive "error" on an otherwise-healthy
  reconciliation, caught by `tests/test_create.py`'s new error-reporting test race — fixed by
  catching `ApiException` with `.status == 404` and treating it the same as "not ready yet" instead
  of letting it propagate.
- `k8s.ensure_sysdba_secret` must be idempotent across kopf handler retries: it generates a random
  password, but if the secret already exists (409, from a prior attempt) it must re-read that
  secret's *actual* stored password rather than using the freshly generated one that was never
  written anywhere — otherwise a retry uses a password that no longer matches the live server.
- The `kubernetes` client's generated `patch_namespaced_*` methods (e.g.
  `patch_namespaced_service`/`patch_namespaced_stateful_set`, used by `update_fn`) default a
  dict-bodied `PATCH` to `Content-Type: application/json-patch+json` — which expects a JSON-Patch
  *list* of operations, not a merge dict — unless `_content_type="application/merge-patch+json"`
  (or `.../strategic-merge-patch+json`) is passed explicitly. `CustomObjectsApi.patch_namespaced_*`
  is unaffected (it already defaults to merge-patch).
- `gsec -user SYSDBA -password <old> -modify SYSDBA -pw <new>` (no `-database` needed — it talks to
  the running server via the Services API, unlike `isql`'s embedded-vs-network distinction) is how
  `firebird.change_sysdba_password` rotates the live SYSDBA password; like `isql -quiet`, it prints
  nothing on success, so any non-empty output is treated as a failure.
- `run_isql`/`change_sysdba_password` both build a shell command embedding the SYSDBA password
  inline (for `run_isql`, twice over for the SYSDBA-readiness probe: once in the `-password`
  switch, once in the `CREATE DATABASE ... PASSWORD '...'` SQL text it echoes into `isql`), and both
  log that exact command at `DEBUG` for troubleshooting. `firebird._redact()` strips every literal
  password value out of the string before it's logged — added after noticing the debug log would
  otherwise leak live passwords into whatever aggregates the operator's stdout, however verbose
  logging is configured.
- `k8s.generate_password` rejects passwords starting with `-`: `secrets.token_urlsafe`'s alphabet
  includes `-`/`_`, and roughly 1 in 64 generated passwords starts with `-` — `shlex.quote` doesn't
  add quotes around it (a leading hyphen isn't a shell metacharacter), so `gsec`/`isql` then parse
  e.g. `-password -abc...` as *two* switches instead of an option and its value, failing with
  "invalid switch specified". Hit this for real: `test_update_sysdba_secret_password_autogenerated`
  failed intermittently (only when the random password happened to start with `-`) until this was
  fixed at the source.
- `CREATE DATABASE` cannot be delegated to a non-SYSDBA user under any circumstances found so far —
  confirmed empirically by granting a user `ADMIN ROLE` (both via SQL's `CREATE USER ... GRANT
  ADMIN ROLE` and `gsec -admin yes`) and having `gsec -display` show them as `admin`, then still
  getting `no permission for CREATE access to DATABASE ...` when that user tried `CREATE DATABASE`.
  `RDB$ADMIN` membership doesn't even grant DML/DDL rights on a database that user didn't create
  (`CREATE TABLE` as that user, against a SYSDBA-created database, still fails with `no privilege
  for this operation`) — this is why `firebird.create_database`/`create_shadow` always hardcode
  `USER 'SYSDBA'`, and why there is currently no `authentication.user`-style feature for wiring a
  non-SYSDBA user into database provisioning (it was tried and reverted — see git history around
  this comment if resurrecting it).
- `security.db` (Firebird's built-in alias for its own security database, where `CREATE USER`
  lives) has `RemoteAccess = false` in `/opt/firebird/databases.conf` by default, so
  `isql ... localhost:security.db` — the "always connect via `localhost:`" rule every `isql` call
  here follows, specifically to avoid racing the running SuperServer for an exclusive lock on the
  security database through the embedded provider — can't reach it at all (`no permission for
  remote access to database security.db`). Connecting to it locally/embedded instead hits that
  exact lock race (confirmed empirically: `Database already opened with engine instance,
  incompatible with current`) *unless* done before the server itself has started (which is how the
  upstream `firebirdsql/firebird` image's own entrypoint script gets away with a bare
  `isql -b security.db`, and why that trick doesn't transfer to anything kubebird runs via
  `kubectl exec` after the pod is already `Ready`). `gsec` sidesteps the whole problem, since it
  talks to the server via the Services API rather than opening `security.db` as a connection at
  all — the reason `firebird.change_sysdba_password` uses it instead of SQL.
- `k8s.render_databases_conf`'s `security.db` line must reproduce the image's own default entry
  verbatim: `security.db = $(dir_secDb)/security<major>.fdb` (`$(dir_secDb)` is a Firebird
  built-in macro, not a literal path) plus a `{ RemoteAccess = false }` block. A first attempt
  wrote a bare `security.db = security<major>.fdb` (no `$(dir_secDb)/` prefix, no `RemoteAccess`
  block) — confirmed via a throwaway `docker run` against `firebirdsql/firebird:3.0.14` that this
  crash-loops the container immediately: the entrypoint's own `isql -b -user SYSDBA security.db`
  (see below) resolves the bare filename relative to whatever the entrypoint's current working
  directory happens to be at that point, not to Firebird's actual security-database directory, and
  fails with `I/O error ... No such file or directory` before the server ever starts — every
  `create_fn`/`update_fn` run hit this, so the pod never became `Ready`. Reproduced/fixed by
  diffing against the image's own shipped `/opt/firebird/databases.conf` (`docker run --rm
  --entrypoint cat firebirdsql/firebird:<version> /opt/firebird/databases.conf`); confirmed the
  same `$(dir_secDb)/security<major>.fdb` form for Firebird 3, 4, and 5 images.
- A ConfigMap volume is *always* mounted read-only by kubelet — regardless of `subPath` and
  regardless of the volumeMount's own `readOnly` field — so `databases.conf` can't live directly
  on one if `update_fn` needs to rewrite it into an already-running pod (adding a database doesn't
  restart the pod, and a ConfigMap update never propagates into an existing `subPath` mount
  anyway). `firebird.write_databases_conf`'s exec against a ConfigMap-backed `subPath` mount was
  confirmed empirically to fail with `Read-only file system`. Fixed by making the ConfigMap only
  the *seed*: `k8s.build_statefulset` adds an `initContainer` that `cp`s the ConfigMap's
  `databases.conf` onto a plain `emptyDir` on every pod (re)start, and the main container mounts
  *that* `emptyDir` over `k8s.DATABASES_CONF_PATH` via `subPath` — `subPath` itself doesn't force
  read-only, only the ConfigMap/Secret/projected volume types do.

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
tox -e type      # mypy src/kubebird tests
tox -e py3       # uv sync + pytest with coverage (--cov=src/kubebird)
tox -e report    # coverage report + html
tox -e clean     # coverage erase
```

Run the full default suite (`clean, lint, format, type, py3, report`) with plain `tox`; all six
pass cleanly. `[testenv:type]` originally just declared `deps = mypy>=0.991` with no
`skip_install`/`uv sync` step (unlike every other testenv here), so it ran in an isolated venv
missing even `kopf` and `kubernetes` — producing ~25 of its ~31 errors as bogus `import-not-found`
noise. Fixed by making it match `[testenv]`'s pattern (`skip_install = True`, `deps = uv`,
`uv sync --active` before the bare `mypy` command) and adding `mypy` itself to the `dev`
dependency-group, plus `pyyaml`/`types-pyyaml` (tests do `import yaml`) and a
`[[tool.mypy.overrides]] module = "kubernetes.*" ignore_missing_imports = true` in
`pyproject.toml` (`kubernetes` genuinely ships no stubs/`py.typed` marker at all; `kopf` and
`testcontainers` both do, so they only needed the env fix). That left 8 real errors, since fixed,
confined to `create.py`/`update.py`/`delete.py`:
- 4 `@kopf.on.*` handlers were flagged as incompatible with `ChangingFn`, since kopf's
  `ChangingFn.__call__` protocol types every handler kwarg as `namespace: str | None` (cluster-scoped
  resources have none) — our handlers all declared `namespace: str`, which is narrower and so not a
  valid substitute, even though `Instance` (and the `Secret` in `sysdba_secret_update_fn`) are always
  namespaced in practice. Fixed by widening to `namespace: str | None` and narrowing back with
  `if namespace is None: raise kopf.PermanentError(...)` at the top of each handler — not a bare
  `assert`, which `-O`/`PYTHONOPTIMIZE` strips at runtime and isn't kopf-retry-aware.
- `update_fn`/`sysdba_secret_update_fn`'s `old`/`new` params had the exact same shape of problem:
  the protocol types them `BodyEssence | Any | None`, but they were declared `kopf.Body | None` — a
  looser-looking but *not* structurally-compatible type. Confirmed empirically (an isolated repro
  script) that only the literal protocol union satisfies `ChangingFn`; widened to match exactly.
- `update.py`: `_, sysdba_password = k8s.ensure_sysdba_secret(...)` reused `_` as the throwaway
  tuple-unpack target, but `_` was *also* the function's own `**_: Any` catch-all parameter name (a
  `dict[str, Any]`) — reassigning it to a `str` was a genuine type conflict, not a stub issue. Fixed
  by renaming the unpack target to `_secret_name`.
- `sysdba_secret_update_fn`'s `old.get("data")`/`new.get("data")` chain: `BodyEssence` (a
  `TypedDict(total=False)`) only declares `metadata`/`spec`/`status` — `data` is Secret-specific and
  outside that schema, so its `.get()` fell back to a plain `object` with no `.get()` of its own.
  Fixed with explicit `Any`-typed locals (`old_essence`/`new_essence`) before chaining
  `.get("data").get("password")`, matching the same `| Any` escape hatch the protocol itself uses.

`tests/conftest.py` defines a session-scoped `k3s` fixture (via `testcontainers`'s
`K3SContainer`, from `testcontainers.community.k3s` — `testcontainers.k3s` is deprecated) that
starts a real k3s container per test session. `testcontainers` is a dev dependency for this.
`tests/test_k3s.py::test_k3s_config_yaml` only exercises the fixture itself (asserts
`k3s.config_yaml()` returns a valid kubeconfig).

`tests/test_k3s.py::test_operator_yaml_deploys_and_grants_expected_rbac` applies `deploy/crd.yaml`
and every object in `deploy/operator.yaml` (Namespace/ServiceAccount/ClusterRole/
ClusterRoleBinding/Role/RoleBinding/Deployment, dispatched by `kind` since it's multi-document
YAML — each namespaced object's target namespace is read from its own `metadata.namespace`, i.e.
`kubebird-system`, rather than a namespace the test picks) via the `kubernetes` client, then uses
`AuthorizationV1Api.create_subject_access_review` (the API `kubectl auth can-i --as=...` itself
calls) to confirm the ServiceAccount actually gets every permission
`create_fn`/`update_fn`/`k8s.py`/`firebird.py` call for, plus kopf's own cluster-scoped framework
needs. This only validates the manifest/RBAC, not full pod scheduling (the Deployment's
`image: quay.io/kubebird/operator:latest` isn't expected to actually be pullable here), so it's
fast (~15s) compared to the `test_create`/`test_update`/`test_delete` suites below.

Gotcha: on cgroup v2 hosts using the systemd cgroup driver, Docker gives the k3s container its own
private cgroup namespace, which its embedded kubelet can't reconcile with the host cgroup paths
bind-mounted in — *every* pod (even built-in ones like coredns) then stays `Pending` forever with
`FailedCreatePodSandBox: ... cgroup.procs: no such file or directory`. The `k3s` fixture works
around this by forcing `container._kwargs["cgroupns"] = "host"` before starting it (a private
attribute — `K3SContainer` has no public API for extra `docker run` kwargs).

`tests/conftest.py` also defines a function-scoped `kubeconfig` fixture, built on top of `k3s`,
that writes the container's kubeconfig to a temp file, sets `KUBECONFIG` to it, and yields the
path (deleting the file on teardown). `tests/test_create.py` uses it for functional tests per
kopf's [testing docs](https://docs.kopf.dev/en/stable/testing/): each applies `deploy/crd.yaml`
and a variant of `deploy/cr.yaml` via the `kubernetes` client library (a direct, non-dev dependency
— used here as the test client and, incidentally, by kopf itself as an optional auth piggyback,
see below), runs the operator in-process with `kopf.testing.KopfRunner(["run", ..., "-m",
"kubebird.create"])`, waits for `status.phase` to reach `Ready` (up to 420s — provisioning involves
an image pull, a real Firebird startup, and one expected retry while waiting for SYSDBA auth to
become live), and execs into the pod to confirm the relevant database file actually exists on disk
before deleting the `Instance`. These are real, non-mocked runs — each takes roughly a minute
end-to-end on a warm image cache.

There are three such tests, run independently against the same `Instance` CRD:
- `test_create_instance` — the CR as shipped in `deploy/cr.yaml`; checks that the primary
  (non-shadow) database file exists under `k8s.DATA_MOUNT_PATH`.
- `test_create_instance_shadow_database` — the same CR but with `metadata.name` overridden to
  `test-shadow` (so it can't collide with the other test's still-being-garbage-collected objects);
  checks that the shadow database's `.shadow` file exists under `k8s.SHADOW_MOUNT_PATH`.
- `test_create_instance_reports_error_in_status` — a database with `shadow: true` but
  `spec.storage.shadow` deleted from the CR, a real (not mocked) way to trigger `create_fn`'s
  `kopf.PermanentError` for that case; polls for `status.error` to contain that specific message
  (`_wait_status_error(..., contains=...)`) instead of `status.phase == "Ready"`, since this CR is
  expected to never reach `Ready`. Polling for *any* non-empty `status.error` (an earlier version
  of this test/helper) is a real false-positive trap here — see the `wait_for_pod_ready` 404 gotcha
  above — since a transient, unrelated error from an earlier retry can still be sitting in
  `status.error` when the deterministic one this test actually means to catch hasn't landed yet.

Since the CRD is cluster-scoped, both tests need it created but only one can actually create it;
`_ensure_crd_established` (not `_wait_established` — renamed when this became shared) tolerates a
409 from `create_custom_resource_definition` for exactly this reason.

`tests/test_update.py` covers `update_fn`/`sysdba_secret_update_fn` the same way, importing
`CR_PATH`/`CRD_PATH`/`_ensure_crd_established`/`_wait_ready`/`_assert_database_file_exists` directly
from `test_create` (pytest's default "rootless" import mode makes each test file a top-level
module, so `from test_create import ...` — not `tests.test_create` — is what actually resolves).
Its `KopfRunner` invocations load both `-m kubebird.create -m kubebird.update` (`click`'s `-m`
option is `multiple=True`), since an update needs `create_fn` to have provisioned the `Instance`
first. Five tests, each patching a freshly `_wait_ready`-confirmed `Instance` and then waiting for
the change to actually land before asserting anything:
- `test_update_instance_service_type` / `test_update_instance_version` — patch
  `spec.service.type`/`spec.version` and check the live `Service`/`StatefulSet` directly.
- `test_update_instance_service_port` — patches `spec.service.port` and checks the live `Service`'s
  `port` changed while `targetPort` stayed at `k8s.FIREBIRD_PORT`.
- `test_update_instance_add_database` — patches `spec.databases` to add an entry and checks the new
  `.fdb` file exists on the pod.
- `test_update_sysdba_secret_password_autogenerated` / `_secretref` — rotate the SYSDBA secret
  (auto-generated, and a manually-created one wired via `secretRef`, respectively) and confirm the
  *live* server now accepts the new password (`firebird.wait_for_sysdba_ready`).

Each of these polls `status.message == "Instance updated."` (`_wait_status_message`) rather than
reusing `_wait_ready`'s `status.phase == "Ready"` check after the patch: since `create_fn` already
left `status.phase` at `"Ready"`, polling for that alone would return on the very first check —
*before* `update_fn` has actually run — letting the test's own assertions race the handler. This
was a real, previously-shipped bug in `test_update_instance_add_database` (it looked flaky/broken
even though `update_fn` itself was correct) — diagnosed by confirming the file *did* exist
immediately after `firebird.create_database`'s own exec, within the same handler invocation, but
was gone by the time the test's later, separate exec checked for it.

`tests/test_delete.py` covers `delete_fn` the same way (one test, `test_delete_instance`), loading
`-m kubebird.create -m kubebird.delete`: creates the `Instance`, waits `Ready`, deletes it, then
waits for the `Instance` object itself to actually disappear (`_wait_gone`, polling for a 404 —
not just for the delete call to return, since the finalizer keeps it present until `delete_fn`
completes) and for the owned `StatefulSet` to be garbage-collected (`_wait_statefulset_gone`).

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
spec:
  image: firebirdsql/firebird
  version: 3.0.14
  databases:
    - name: "instance.fdb"
      alias: "" # if empty, "name" is used as the alias
      shadow: false
  service:
    type: ClusterIP
  storage:
    primary:
      class: ""
      size: 3Gi
    shadow:
      class: ""
      size: 3Gi
  authentication:
    sysdba:
      secretRef: ""
```

Reconciling this CR (`create_fn` in `src/kubebird/create.py`):

- Deploys the Firebird instance as a `StatefulSet` (1 replica) using the given `image`/`version`,
  mounting the primary-data PVC at `k8s.DATA_MOUNT_PATH` (`/var/lib/firebird/data`) and, only if
  `storage.shadow` is set, a second PVC at `k8s.SHADOW_MOUNT_PATH` (`/var/lib/firebird/shadow`).
- Creates a `Service` for the instance (type from `spec.service.type`, default `ClusterIP`),
  exposing `spec.service.port` (default `3050`, from `k8s.FIREBIRD_PORT`) as its `port`; `targetPort`
  is always `k8s.FIREBIRD_PORT` since that's the container's actual, non-configurable listening
  port — only the Service-facing port is adjustable.
- Creates a `PVC` for `storage.primary` (required) and, if present, a second one for
  `storage.shadow`, each sized per `.size` and using the cluster default `StorageClass` when
  `.class` is empty. Both are plain PVCs referenced by the pod's `volumes`, not
  `volumeClaimTemplate`s, since the CR describes exactly one instance/pod.
- Instantiates every entry in `databases` via `isql` exec'd into the pod
  (`CREATE DATABASE ... PAGE_SIZE <pageSize> DEFAULT CHARACTER SET <charset> COLLATION
  <collation>`, using each entry's `pageSize`/`charset`/`collation` — defaulted by the CRD to
  `8192`/`UTF8`/`UTF8` — via `database.get(..., <default>)` in `create_fn`/`update_fn`, so this
  still works even for a hand-built dict that skips the API server's defaulting), creating a
  `CREATE SHADOW 1` file under the shadow mount for any entry with `shadow: true` — raising a
  `kopf.PermanentError` if `storage.shadow` isn't configured in that case.
- Manages a Firebird alias per `databases` entry (`database["alias"]` if set, otherwise the
  database's own `name`, e.g. `instance.fdb` — `database.get("alias") or name` in
  `k8s.render_databases_conf` — pointing at the full path under `k8s.DATA_MOUNT_PATH` derived from
  `name` either way) in `/opt/firebird/databases.conf`, so clients can connect using that alias
  instead of needing to know the in-pod mount path. `create_fn` builds a
  `<instance-name>-databases-conf` ConfigMap (`k8s.build_databases_conf_configmap`/
  `render_databases_conf`) from the full `spec.databases` list plus a version-specific
  `security.db` alias (see the `databases.conf` gotchas above — this file replaces the image's own
  default `databases.conf` wholesale, so `security.db` must be replicated or SYSDBA authentication
  itself breaks) *before* creating the `StatefulSet`, so a fresh pod always starts with the correct
  content. `build_statefulset` wires this ConfigMap in via an `initContainer` + writable `emptyDir`
  rather than mounting it directly (see the same gotchas), and labels/adopts it like every other
  created object.
- Manages authentication: if `authentication.sysdba.secretRef` is unset, generates a
  `<instance-name>-sysdba` secret (keys `username: SYSDBA`, `password: <random>`) and wires it
  into the `StatefulSet` via `FIREBIRD_ROOT_PASSWORD`/`secretKeyRef`; if set, reads that secret's
  `password` key instead. These `username`/`password` key names are a kubebird convention, not
  documented anywhere else — keep README.md's authentication bullets in sync if this ever changes.
  There is no way to provision a non-SYSDBA user yet (see gotcha above on why `CREATE DATABASE`,
  and object-level access on a database it didn't create, can't be delegated away from SYSDBA).
- Adopts every created object with `kopf.adopt()`, so deleting the `Instance` garbage-collects them
  via owner references.
- Labels every created object (`PVC`(s), `Service`, `StatefulSet`, and the auto-generated SYSDBA
  `Secret`) with `k8s.INSTANCE_LABEL` (`kubebird.github.io/instance: <name>`) — e.g.
  `kubectl get all,pvc,secrets -l kubebird.github.io/instance=<name>` finds everything for one
  `Instance`. `build_service`/`build_statefulset` already needed this label internally for the
  Service→Pod selector and the StatefulSet's pod template; it's now also stamped on each object's
  own `metadata.labels`, not just used internally.
- Reports any handler failure into `status.error` (cleared on success) — see the `_reconcile()`
  bullet under "Project overview" above — surfaced via `deploy/crd.yaml`'s `Error` printer column,
  so `kubectl get instances` shows e.g. an RBAC-forbidden error or a missing-`storage.shadow`
  `kopf.PermanentError` directly, without needing to read the operator pod's own logs.

`storage.primary.size` and `storage.shadow.size` in `deploy/crd.yaml` carry a `pattern` validating
the standard Kubernetes resource-quantity grammar (e.g. `3Gi`, `500Mi`, `1.5G`), so the API server
itself rejects malformed sizes (e.g. `"banana"`) with a 422 before `create_fn` ever runs.

Updating an `Instance` (`update_fn` in `src/kubebird/update.py`, `@kopf.on.update` on `Instance`):

- Re-resolves the SYSDBA password the same way `create_fn` does (`k8s.ensure_sysdba_secret`), then
  unconditionally patches the `<instance-name>-databases-conf` ConfigMap with `databases.conf`
  content re-rendered from the *current* `spec.databases`/`spec.version` (not a diff — the file is
  always regenerated in full), and unconditionally reconciles the `Service`'s type/port and the
  `StatefulSet`'s container image/version via
  `patch_namespaced_service`/`patch_namespaced_stateful_set` (idempotent no-ops when unchanged). The
  `Service` patch always sends the full `ports` list (a JSON merge-patch replaces arrays wholesale
  rather than merging by index/name), rebuilt from `spec.service.port`/`k8s.FIREBIRD_PORT` the same
  way `k8s.build_service` does.
- Waits for the pod to be `Ready` and SYSDBA-live again (relevant when `spec.version` changed and
  the `StatefulSet` rolled the pod), then execs the same freshly-rendered `databases.conf` content
  directly into the running container (`firebird.write_databases_conf`) — necessary even though the
  ConfigMap was just patched, since adding a database doesn't restart the pod, and a ConfigMap
  update never reaches an already-mounted `subPath` on its own (see the gotcha above).
- Diffs `old.spec.databases` (kopf's own kwarg, the previously-handled body) against
  `spec.databases` and provisions only the newly-added entries — pre-existing ones are left alone.
- Reports progress via `status.phase`/`status.message` the same way `create_fn` does (and any
  handler failure into `status.error`, also the same way), ending in
  `status.message == "Instance updated."` (a distinct value from `create_fn`'s `"Instance
  provisioned."`, useful for tests/tooling to tell a genuine update apart from a stale `Ready`
  status left over from creation).

Rotating the SYSDBA secret's password (`sysdba_secret_update_fn` in `src/kubebird/update.py`,
`@kopf.on.update` on core `Secret`, filtered by `labels={"kubebird.github.io/role": "sysdba"}`):

- Compares the base64 `data.password` between kopf's `old`/`new` kwargs; no-ops if either is
  missing (secret just created) or unchanged (some other field on the secret changed).
- Otherwise decodes both and calls `firebird.change_sysdba_password` (`gsec`) against
  `<instance-name>-0`, using the *old* password to authenticate and set the *new* one — so the live
  server and the secret never drift apart.
- The instance name comes from the secret's own `kubebird.github.io/instance` label, not from its
  name, since a user-provided `authentication.sysdba.secretRef` secret can be named anything.
- The auto-generated `<instance-name>-sysdba` secret gets both labels
  (`kubebird.github.io/instance`, `kubebird.github.io/role: sysdba`) at creation. A user-provided
  `secretRef` secret is *not* owned by kubebird (no `kopf.adopt()`), but `k8s.ensure_sysdba_secret`
  still labels it the same way (`k8s._label_sysdba_secret`) on every reconcile, purely so this watch
  covers it too — if the same secret is referenced by more than one `Instance`, the
  `kubebird.github.io/instance` label just reflects whichever one reconciled it last.

Deleting an `Instance` (`delete_fn` in `src/kubebird/delete.py`, `@kopf.on.delete` on `Instance`):
logs and sets `status.phase = "Deleting"`, then returns. Registering any `@kopf.on.delete` handler
at all is what makes kopf attach the `kopf.zalando.org/KopfFinalizerMarker` finalizer to the
`Instance` in the first place, so the object stays present (with `metadata.deletionTimestamp` set)
until this handler returns without raising; kopf then drops the finalizer, the `Instance` actually
disappears, and Kubernetes garbage-collects every `kopf.adopt()`-ed object through its owner
references — the same outcome as before `delete_fn` existed, just no longer racing the `Instance`
object's own removal against that garbage collection.

`deploy/crd.yaml` holds the `CustomResourceDefinition` (OpenAPI v3 schema for the `spec`/`status`
shape above) and `deploy/cr.yaml` holds a sample `Instance` matching it. Keep both files in sync
with each other and with the README's sample whenever the CR shape changes.

`deploy/operator.yaml` is the Namespace/Deployment/RBAC manifest for running the operator itself
in-cluster (the README's Installation section documents a plain `kubectl apply -f deploy/crd.yaml
-f deploy/operator.yaml`, no `-n` flag needed), following kopf's own
[deployment](https://docs.kopf.dev/en/stable/deployment/)/[RBAC](https://docs.kopf.dev/en/stable/rbac/)
guidance for a namespace-scoped operator:
- A `Namespace` object creates a fixed `kubebird-system` namespace, and every other namespaced
  object in the file (`ServiceAccount`/`Role`/`RoleBinding`/`Deployment`) hardcodes
  `metadata.namespace: kubebird-system` to match — rather than the previous design of leaving them
  namespace-less and relying on `kubectl apply -n <namespace>` to pick one at apply-time. This is
  what lets the `ClusterRoleBinding`'s `subjects[].namespace` (see below) be correct by construction
  instead of a manual edit someone has to remember to keep in sync. The tradeoff: `Instance` CRs
  must now be created in `kubebird-system` too, since the namespaced `Role`/`RoleBinding` only grant
  access there.
- A `ServiceAccount` plus that namespaced `Role`/`RoleBinding` granting exactly what the code above
  calls: `get`/`list`/`watch`/`patch` on `instances` and `instances/status`; `get`/`list`/`watch`/
  `create`/`patch` on `secrets` (covers `k8s.ensure_sysdba_secret`'s create-or-reuse, the labeling
  of user-provided `secretRef` secrets, and `sysdba_secret_update_fn`'s watch); `create`/`patch` on
  `configmaps` (the `databases.conf` ConfigMap `create_fn`/`update_fn` build/reconcile); `create` on
  `persistentvolumeclaims`; `create`/`patch` on `services` and `statefulsets`; `get` on `pods` and
  `create` on `pods/exec` (the `isql`/`gsec` provisioning in `firebird.py`); `create` on `events`;
  and a `kopfpeerings` rule kopf's own docs recommend (a harmless no-op unless a `KopfPeering` CRD
  is separately installed — kubebird runs a single replica, so peering itself isn't required).
- A thin cluster-scoped `ClusterRole`/`ClusterRoleBinding`, still needed even though the operator
  itself is namespace-scoped: kopf's framework needs `list`/`watch` on `customresourcedefinitions`
  and `namespaces`, both genuinely cluster-scoped resource types with no namespaced equivalent to
  grant instead. No webhook-config permissions, since kubebird registers no admission handlers. The
  `ClusterRoleBinding`'s `subjects[].namespace` is `kubebird-system` — unlike every other namespaced
  object here, a `ClusterRoleBinding` has no "current namespace" for Kubernetes to infer at
  apply-time no matter how the rest of the file is structured, so this value must always be spelled
  out explicitly; fixing the operator's own namespace means there's exactly one correct value for it
  instead of a moving target.
- The `Deployment` runs 1 replica with `strategy: Recreate` (matching kopf's own "never run two
  operators for the same objects" recommendation) and wires `NAMESPACE` via the Downward API
  (`fieldRef: metadata.namespace`, i.e. `kubebird-system`), which `src/kubebird/operator.py`'s
  existing namespace-scoping logic already reads, plus a fixed `LOG_LEVEL: INFO` that same file's
  logging-configuration logic reads.

Verified against a real cluster (applying the whole file, including the `Namespace` object, from
scratch), plus `kubectl auth can-i --as=system:serviceaccount:...` for every rule above; see
`tests/test_k3s.py::test_operator_yaml_deploys_and_grants_expected_rbac`.

## Container image

`Dockerfile` builds the operator into a container image via three stages, all based on the same
`registry.access.redhat.com/ubi10/python-314-minimal` image:
- `build` — creates a venv, installs `uv`, and runs `uv build --wheel` to produce
  `dist/kubebird-<version>-py3-none-any.whl` from `pyproject.toml`/`src/`.
- `user` — the minimal image ships no `useradd` (no `shadow-utils` package at all), so this stage
  runs `microdnf install -y shadow-utils` first, then creates `appuser` (uid 8877). The final
  stage only copies its resulting `/etc/passwd`/`/etc/group` over, not the installed package, to
  avoid carrying `shadow-utils` (and its `microdnf` cache) into the final image.
- The final (unnamed) stage installs the wheel from `build` into its own venv via
  `pip install --no-cache-dir /app/*.whl` — a wildcard, not a version-pinned filename, so the
  `VERSION` build-arg only affects the `LABEL version=$VERSION` metadata and is otherwise optional;
  it used to also gate finding the right wheel file (`kubebird-${VERSION}-py3-none-any.whl`), which
  broke the build entirely if omitted or mismatched. `chown`s `/app` to `appuser` and runs as that
  non-root user from then on.
- `CMD` is `/app/venv/bin/kubebird-operator` — must match `[project.scripts]`'s key exactly
  (`kubebird-operator`, hyphenated). `pip install`s a wheel's console-script entry points under
  that exact key, not the underscored module path it maps to (`kubebird.operator:main`) — a
  previous version of this Dockerfile had `CMD ["/app/venv/bin/kubebird_operator"]` (underscore),
  which doesn't exist and fails at container start with "exec format error"/"no such file".

Build with `docker build --build-arg VERSION=<version> -t kubebird:<version> .`. Verified against
a real `docker build`+`docker run`: the image builds, `id`/`whoami` inside it report `appuser`
(uid 8877, not root), and `kubebird-operator` starts and gets exactly as far as attempting
kopf's cluster login (expected to fail outside an actual cluster/kubeconfig).

## CI

`.github/workflows/ci.yml` triggers on `push`, on `pull_request` (`types: [opened, synchronize,
reopened]`), and on `pull_request_review` (`types: [submitted]`). The two jobs are mutually
exclusive by trigger — `test` explicitly excludes tag pushes, `release` explicitly requires one —
so a tag push runs only `release`, with no test gate of its own:
- `test` job — `if: (github.event_name == 'push' && github.ref_type != 'tag') ||
  github.event_name == 'pull_request' || github.event.review.state == 'approved'`, so it runs for
  every branch push, every time a PR is opened or gets a new commit pushed to it (`synchronize`) or
  is reopened, and for an "approved" PR review, but *not* for a tag push, and not for a "changes
  requested"/"commented" review. Checks out `github.event.pull_request.head.sha || github.sha` (the
  PR's own head commit for a `pull_request`/`pull_request_review` event, otherwise the pushed
  commit), then `actions/setup-python` (3.14) + `pip install tox` + plain `tox` — the same full
  `clean, lint, format, type, py3, report` suite as "Development commands" above, including the
  real k3s/testcontainers end-to-end tests (Docker is preinstalled on GitHub-hosted Linux runners,
  so no extra setup needed for that).
- `release` job — `if: github.event_name == 'push' && github.ref_type == 'tag'`, so it only ever
  runs for a tag push (never a branch push or a PR event) — deliberately with no `needs: test` and
  no test run of its own: by the time a commit is tagged, it should already have gone through
  `test` via whichever push/PR landed it on `main`, so re-testing the exact tagged commit here would
  be redundant. Logs into `quay.io` via `docker/login-action` using repo secrets
  `QUAY_USERNAME`/`QUAY_PASSWORD` (a quay.io robot account's token works well as the latter), then
  `docker/build-push-action` builds this repo's `Dockerfile` and pushes `quay.io/kubebird/operator`
  tagged both `:latest` and with the tag name itself (e.g. `:v1.2.3`) — a plain branch push no
  longer publishes any image at all. The tag name is also passed as the `VERSION` build-arg
  (cosmetic only — see "Container image" above).

## Requirements

Python >= 3.14 (see `.python-version`, pinned to 3.14). `uv run kubebird-operator` (or
`NAMESPACE=kubebird-system LOG_LEVEL=DEBUG uv run kubebird-operator` to scope it to one namespace
and/or change log verbosity) runs the operator via the `kubebird-operator` console script
(`src/kubebird/operator.py`, on `uvloop`); the tests themselves still drive it via
`kopf.testing.KopfRunner` and `-m kubebird.<module>` instead (see "Development commands" above),
not through this entry point.
