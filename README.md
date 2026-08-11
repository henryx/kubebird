# Kubebird

## Overview
[Kubebird](github.com/henryx/kubebird) is a Kubernetes operator based on Python and `kopf` to install and manage [Firebird RDBMS](https://firebirdsql.org/) instances

## Architecture
Project uses the namespaced CR `Instances` that defines Firebird instance.

This is a sample of `Instances`:
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
      pageSize: 8192 # defaults to 8192; one of 4096, 8192, 16384
      charset: UTF8 # defaults to UTF8
      collation: UTF8 # defaults to UTF8
    - name: "shadowed.fdb"
      shadow: true
  service:
    type: ClusterIP
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

With this CR, Kubebird can:
- Deploy an instance of Firebird, in a StatefulSet mode using `image` and `version` specified. Deployment is made in default namespace.
- Create a service for the instance. Default service type is `ClusterIP`.
- Define the PVC used for the instance's primary data (`storage.primary`) with specified size and storage class. If storage class isn't specified, it uses the default storage class. Size must be a valid Kubernetes quantity (e.g. `3Gi`, `500Mi`); the CRD rejects anything else.
- Declare a list of the databases managed by instance. Based by of the configuration, database can be instantiated in shadow mode; shadow files live on a second, separate PVC (`storage.shadow`), which is required if any database has `shadow: true`. Each database can also set `pageSize` (one of `4096`, `8192`, `16384`; defaults to `8192`), `charset` and `collation` (both default to `UTF8`).
- Authentication section is optional. If is specified, you can:
  - Declare SYSDBA database password using a secret. If secrets isn't specified, operator create a `<instance-name>-sysdba` secret with a random password. The secret has `username` (always `SYSDBA`) and `password` keys.
- Label every object it creates (PVCs, Service, StatefulSet, and the SYSDBA secret) with `kubebird.github.io/instance: <name>`, so `kubectl get all,pvc,secrets -l kubebird.github.io/instance=<name>` finds everything for one `Instance`.

Kubebird also reacts to updates on an existing `Instance`:
- Changing `spec.service.type` or `spec.version` reconciles the `Service`/`StatefulSet` in place.
- Adding an entry to `spec.databases` provisions just that new database (existing ones are left
  alone).
- Rotating the SYSDBA secret's password (the auto-generated one, or a user-provided
  `authentication.sysdba.secretRef`) pushes the new password to the live server automatically, so
  the secret and the running instance never drift apart.

Deleting an `Instance` relies on Kubernetes garbage collection of the objects Kubebird created for
it (they're all owned by the `Instance`); the operator itself just logs the deletion and reports
`status.phase: Deleting` while that garbage collection runs.

## Installation

To install Kubebird in the Kubernetes cluster, you can use these commands:
```bash
kubectl apply -f deploy/crd.yaml
kubectl apply -f deploy/operator.yaml
```

The operator itself runs via the `kubebird-operator` console script (on `uvloop`):
```bash
uv run kubebird-operator
# or, to scope it to a single namespace instead of the whole cluster:
NAMESPACE=default uv run kubebird-operator
```

To build the container image instead:
```bash
docker build --build-arg VERSION=0.1.0 -t kubebird:0.1.0 .
```
The image runs as a non-root `appuser` (uid 8877) on a Red Hat UBI10 base and starts
`kubebird-operator` by default.


## Development Commands
```bash
# Setup
$ uv init --name Kubebird --app --description "Kubebird - A Kubernetes operator for Firebird" --build-backend uv --no-readme
$ uv add kopf kubernetes uvloop
$ uv add --dev pytest pytest-cov tox ruff mypy pyyaml types-pyyaml
$ uv add --dev testcontainers
```
For e2e tests, `testcontainers` is used to run a k3s cluster: `tests/conftest.py` defines a
session-scoped `k3s` fixture (`testcontainers.community.k3s.K3SContainer`), tested on its own in
`tests/test_k3s.py`. A `kubeconfig` fixture builds on it to point the `kubernetes` client library
at the container.

`tests/test_create.py` uses that fixture, together with [kopf's testing
utilities](https://docs.kopf.dev/en/stable/testing/), to apply `deploy/crd.yaml` and
`deploy/cr.yaml` against the k3s cluster and run the operator in-process via
`kopf.testing.KopfRunner`. These are real end-to-end runs: each waits for `status.phase` to reach
`Ready` (StatefulSet + real Firebird image pull + database provisioning over `isql`), then execs
into the pod to confirm the relevant database file actually exists on disk before deleting the
`Instance`. There are two such tests: `test_create_instance` checks the primary database, and
`test_create_instance_shadow_database` checks that a database with `shadow: true` gets its shadow
file on the separate shadow PVC.

`tests/test_update.py` covers the update-reconciliation behaviour the same way: patching an
already-`Ready` `Instance`'s `service.type`/`version`/`databases`, and rotating its SYSDBA secret's
password (both the auto-generated one and a user-provided `secretRef`), then confirming the change
actually took effect against the live `Service`/`StatefulSet`/pod.

`tests/test_delete.py` covers deletion the same way: deletes a `Ready` `Instance` and confirms it
actually disappears (not just that the delete call returned) and that Kubernetes garbage-collects
the `StatefulSet` it owned.

## License

Kubebird is licensed under the [Apache License 2.0](LICENSE).
