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
  service:
    type: ClusterIP
  storage:
    class: "" # if empty, uses the default storage class
    size: 3Gi
  authentication:
    sysdba:
      secretRef: ""
    user:
      secretRef: ""
```

With this CR, Kubebird can:
- Deploy an instance of Firebird, in a StatefulSet mode using `image` and `version` specified. Deployment is made in default namespace.
- Create a service for the instance. Default service type is `ClusterIP`.
- Define the PVC used for the instance with specified size and storage class. If storage class isn't specified, it uses the default storage class.
- Declare a list of the databases managed by instance. Based by of the configuration, database can be instantiated in shadow mode.
- Authentication section is optional. If is specified, you can:
  - Declare a user using a secret. If secret isn't specified, no other users than SYSDBA are created. The secret must have `username` and `password` keys.
  - Declare SYSDBA database password using a secret. If secrets isn't specified, operator create a `<instance-name>-sysdba` secret with a random password. The secret has `username` (always `SYSDBA`) and `password` keys.

There is no update/delete reconciliation yet: deleting an `Instance` relies on Kubernetes garbage
collection of the objects Kubebird created for it (they're all owned by the `Instance`).

## Installation

To install Kubebird in the Kubernetes cluster, you can use these commands:
```bash
kubectl apply -f deploy/crd.yaml
kubectl apply -f deploy/operator.yaml
```


## Development Commands
```bash
# Setup
$ uv init --name Kubebird --app --description "Kubebird - A Kubernetes operator for Firebird" --build-backend uv --no-readme
$ uv add kopf kubernetes
$ uv add --dev pytest pytest-cov tox ruff
$ uv add --dev testcontainers
```
For e2e tests, `testcontainers` is used to run a k3s cluster: `tests/conftest.py` defines a
session-scoped `k3s` fixture (`testcontainers.community.k3s.K3SContainer`), tested on its own in
`tests/test_k3s.py`. A `kubeconfig` fixture builds on it to point the `kubernetes` client library
at the container.

`tests/test_create.py` uses that fixture, together with [kopf's testing
utilities](https://docs.kopf.dev/en/stable/testing/), to apply `deploy/crd.yaml` and
`deploy/cr.yaml` against the k3s cluster and run the operator in-process via
`kopf.testing.KopfRunner`. It's a real end-to-end run: it waits for `status.phase` to reach
`Ready` (StatefulSet + real Firebird image pull + database provisioning over `isql`), then execs
into the pod to confirm the database file actually exists on disk before deleting the `Instance`.

## License

Kubebird is licensed under the [Apache License 2.0](LICENSE).
