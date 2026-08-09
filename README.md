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
  - Declare a user using a secret. If secret isn't specified, no other users than SYSDBA are created.
  - Declare SYSDBA database password using a secret. If secrets isn't specified, operator create a `<instance-name>-sysdba` secret with a random password.



## Development Commands
```bash
# Setup
$ uv init --name Kubebird --app --description "Kubebird - A Kubernetes operator for Firebird" --build-backend uv --no-readme
$ uv add kopf
$ uv add --dev pytest pytest-cov tox ruff
```
For e2e tests, is used testcontainers with k3s image, and deployed an `Instances` with 1 pod

