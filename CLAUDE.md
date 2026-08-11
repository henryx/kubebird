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
are declared dependencies. Still missing: a `deploy/operator.yaml` for running the operator
in-cluster (Deployment + RBAC), and `update_fn` support for changes to `authentication`/`storage`
(only `service`/`version`/`databases` are reconciled today).

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
  `ProvisioningDatabases` → `Ready`).
- `src/kubebird/update.py` — `update_fn` (`@kopf.on.update` on `Instance`) reconciles `Service`
  type and `StatefulSet` image/version changes and provisions any databases newly added to
  `spec.databases`; `sysdba_secret_update_fn` (`@kopf.on.update` on core `Secret`, filtered by the
  `kubebird.github.io/role: sysdba` label) pushes a rotated SYSDBA secret password to the live
  server via `gsec`.
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
context), matching `kopf run` with neither `-n` nor `-A`. It is a regular (non-executable) file
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

Run the full default suite (`clean, lint, format, type, py3, report`) with plain `tox`.
`[testenv:type]` originally just declared `deps = mypy>=0.991` with no `skip_install`/`uv sync`
step (unlike every other testenv here), so it ran in an isolated venv missing even `kopf` and
`kubernetes` — producing ~25 of its ~31 errors as bogus `import-not-found` noise. Fixed by making it
match `[testenv]`'s pattern (`skip_install = True`, `deps = uv`, `uv sync --active` before the bare
`mypy` command) and adding `mypy` itself to the `dev` dependency-group, plus `pyyaml`/`types-pyyaml`
(tests do `import yaml`) and a `[[tool.mypy.overrides]] module = "kubernetes.*" ignore_missing_imports
= true` in `pyproject.toml` (`kubernetes` genuinely ships no stubs/`py.typed` marker at all; `kopf`
and `testcontainers` both do, so they only needed the env fix). That leaves 8 real errors, not yet
fixed, confined to `create.py`/`update.py`/`delete.py`:
- 4 `@kopf.on.*` handlers flagged as incompatible with `ChangingFn`, since kopf's
  `ChangingFn.__call__` protocol types every handler kwarg as `namespace: str | None` (cluster-scoped
  resources have none) — our handlers all declare `namespace: str`, which is narrower and so not a
  valid substitute, even though `Instance` (and the `Secret` in `sysdba_secret_update_fn`) are always
  namespaced in practice.
- `update.py`: `_, sysdba_password = k8s.ensure_sysdba_secret(...)` reuses `_` as the throwaway
  tuple-unpack target, but `_` is *also* the function's own `**_: Any` catch-all parameter name (a
  `dict[str, Any]`) — reassigning it to a `str` is a genuine type conflict, not a stub issue.
- `update.py`: three `dict.get()` "no overload variant matches" errors chained off `old.get("spec")`
  and `meta["labels"]`-adjacent code — still investigating whether this is a real narrowing bug or
  another `kopf.Body`/`Meta` (`dicts.MappingView`) stub-overload mismatch.

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

There are two such tests, run independently against the same `Instance` CRD:
- `test_create_instance` — the CR as shipped in `deploy/cr.yaml`; checks that the primary
  (non-shadow) database file exists under `k8s.DATA_MOUNT_PATH`.
- `test_create_instance_shadow_database` — the same CR but with `metadata.name` overridden to
  `test-shadow` (so it can't collide with the other test's still-being-garbage-collected objects);
  checks that the shadow database's `.shadow` file exists under `k8s.SHADOW_MOUNT_PATH`.

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
  targeting port 3050.
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

`storage.primary.size` and `storage.shadow.size` in `deploy/crd.yaml` carry a `pattern` validating
the standard Kubernetes resource-quantity grammar (e.g. `3Gi`, `500Mi`, `1.5G`), so the API server
itself rejects malformed sizes (e.g. `"banana"`) with a 422 before `create_fn` ever runs.

Updating an `Instance` (`update_fn` in `src/kubebird/update.py`, `@kopf.on.update` on `Instance`):

- Re-resolves the SYSDBA password the same way `create_fn` does (`k8s.ensure_sysdba_secret`), then
  unconditionally reconciles the `Service` type and the `StatefulSet`'s container image/version via
  `patch_namespaced_service`/`patch_namespaced_stateful_set` (idempotent no-ops when unchanged).
- Waits for the pod to be `Ready` and SYSDBA-live again (relevant when `spec.version` changed and
  the `StatefulSet` rolled the pod).
- Diffs `old.spec.databases` (kopf's own kwarg, the previously-handled body) against
  `spec.databases` and provisions only the newly-added entries — pre-existing ones are left alone.
- Reports progress via `status.phase`/`status.message` the same way `create_fn` does, ending in
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

The README's Installation section documents `kubectl apply -f deploy/crd.yaml -f deploy/operator.yaml`,
but `deploy/operator.yaml` (the Deployment/RBAC manifest for running the operator itself in-cluster)
does not exist yet — only `crd.yaml` and `cr.yaml` are present under `deploy/`. Now that `create_fn`
is implemented, that RBAC needs to grant (at least): `get`/`list`/`watch`/`patch` on `instances`
(and their `status` subresource) for the CRD's group; `create`/`patch` on `secrets`,
`persistentvolumeclaims`, `services`, `statefulsets`; `get`/`list`/`watch`/`patch` on `secrets`
specifically (`update_fn` re-reads them, `sysdba_secret_update_fn` watches and labels them); `get`
on `pods` and `create` on `pods/exec` (needed for the `isql`/`gsec` provisioning in
`firebird.py`); plus whatever kopf itself needs for peering/events (see kopf's own RBAC docs).

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

## Requirements

Python >= 3.14 (see `.python-version`, pinned to 3.14). `uv run kubebird-operator` (or
`NAMESPACE=default uv run kubebird-operator` to scope it to one namespace) runs the operator via
the `kubebird-operator` console script (`src/kubebird/operator.py`, on `uvloop`); the tests
themselves still drive it via `kopf.testing.KopfRunner` and `-m kubebird.<module>` instead (see
"Development commands" above), not through this entry point.
