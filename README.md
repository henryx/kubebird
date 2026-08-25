# Kubebird

## Overview
[Kubebird](https://github.com/henryx/kubebird) is a Kubernetes operator based on kubebuilder to install and manage [Firebird RDBMS](https://firebirdsql.org/) instances

## Installation

Tagging the repository with a semver tag (e.g. `0.2.0`) triggers the `Release` GitHub Actions
workflow, which builds and pushes the manager image to `quay.io/kubebird/controller` (tagged with
both the release version and `latest`) and publishes a GitHub Release with a consolidated
`install.yaml` (CRD + RBAC + Deployment) attached.

Install the latest release directly:

```bash
kubectl apply -f https://github.com/henryx/kubebird/releases/latest/download/install.yaml
```

This deploys the operator into the `kubebird-system` namespace. The manager requires a
`WATCH_NAMESPACE` env var (set on the Deployment) naming the namespace, or comma-separated list of
namespaces, whose `Instance` resources it should reconcile.

To build the manifest yourself instead: `make build-installer IMG=<your-registry>/controller:<tag>`
produces the same consolidated YAML at `dist/install.yaml`.

## Architecture
Project uses the namespaced CR `Instances` that defines Firebird instance.

This is a sample of `Instances`:
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

With this CR, Kubebird can:
- Deploy an instance of Firebird, in a StatefulSet mode using `image` and `version` specified, in whichever namespace the `Instance` itself is created in.
- Create a service for the instance. Default service type is `ClusterIP`, exposed on `service.port` (defaults to `3050`); the pod's container port is always `3050` regardless of this setting.
- Define the PVC used for the instance's primary data (`storage.primary`) with specified size and storage class. If storage class isn't specified, it uses the default storage class. Size must be a valid Kubernetes quantity (e.g. `3Gi`, `500Mi`); the CRD rejects anything else.
- Declare a list of the databases managed by instance. Based by of the configuration, database can be instantiated in shadow mode; shadow files live on a second, separate PVC (`storage.shadow`), which is required if any database has `shadow: true`. Each database can also set `pageSize` (one of `4096`, `8192`, `16384`; defaults to `8192`), `charset` and `collation` (both default to `UTF8`).
- Register a Firebird alias for each database in `/opt/firebird/databases.conf` using a ConfigMap called `<instance-name>-aliases`, so clients can connect using that alias instead of the in-pod filesystem path. Uses `alias` if set, otherwise falls back to the database's own `name` (e.g. `instance.fdb`).
- Authentication is optional. If `authentication.sysdba.secretRef` is specified, Kubebird uses that Secret for the SYSDBA password; if it isn't specified, Kubebird creates a `<instance-name>-sysdba` secret with a random password. Either way, the secret has `username` (always `SYSDBA`) and `password` keys.
- Label every object it creates (PVCs, Service, StatefulSet, the aliases ConfigMap, and the SYSDBA secret) with `kubebird.github.io/instance: <name>`, so `kubectl get all,pvc,secrets,configmaps -l kubebird.github.io/instance=<name>` finds everything for one `Instance`.
- Report the most recent error, if any, in `status.error` — surfaced without needing to check the operator's own logs, via the `MESSAGE` column below. It's cleared automatically once the `Instance` reconciles successfully again.
- Surface `kubectl get instances` columns beyond the default `NAME`/`AGE`: `VERSION` (the Firebird version deployed, from `spec.version`), `STATUS` (`Provisioning`, `Ready`, or `Deleting`), and `MESSAGE` (the reconcile error if the last reconcile failed, otherwise why it's currently in that phase).

### Object creation flow

When an `Instance` is created, Kubebird creates the objects below in order (steps 1-4); each one is
owned by the `Instance` and removed automatically when the `Instance` is deleted. Kubernetes then
creates the PVCs and Pod from the `StatefulSet`, and once the Pod becomes ready Kubebird syncs the
SYSDBA password and creates the requested databases inside it (steps 5-7):

```mermaid
flowchart TD
    User(["kubectl apply -f cr.yaml"]) --> CR[/"Instance"/]
    CR --> Kubebird["Kubebird"]

    Kubebird -->|"1"| Secret["Secret<br/>&lt;name&gt;-sysdba"]
    Kubebird -->|"2"| CM["ConfigMap<br/>&lt;name&gt;-aliases"]
    Kubebird -->|"3"| Service["Service<br/>&lt;name&gt;"]
    Kubebird -->|"4"| STS["StatefulSet<br/>&lt;name&gt;"]

    Secret -.->|SYSDBA password| STS
    CM -.->|database aliases| STS
    Service -.->|routes traffic to| STS

    STS -->|Kubernetes creates| PVCPrimary["PVC<br/>primary-&lt;name&gt;-0"]
    STS -->|"Kubernetes creates, optional"| PVCShadow["PVC<br/>shadow-&lt;name&gt;-0"]
    STS -->|Kubernetes creates| Pod["Pod<br/>&lt;name&gt;-0"]

    Kubebird -->|"5: waits for readiness"| Pod
    Kubebird -->|"6: syncs the SYSDBA password"| Pod
    Kubebird -->|"7: creates the databases"| Pod

    classDef owned fill:#e6ecff,stroke:#3355ff,color:#000
    class Secret,CM,Service,STS,PVCPrimary,PVCShadow owned
```

The dotted arrows show how the `StatefulSet` uses the other objects (the SYSDBA password from the
secret, database aliases from the ConfigMap, traffic routing from the Service) rather than a
separate creation step; the PVCs and Pod, by contrast, are created directly by Kubernetes from the
`StatefulSet`'s templates. The shadow `PVC` only exists when `storage.shadow` is set on the
`Instance`.

Kubebird also reacts to updates on an existing `Instance`:
- Changing `spec.service.type`, `spec.service.port`, or `spec.version` reconciles the
  `Service`/`StatefulSet` in place.
- Adding an entry to `spec.databases` provisions just that new database (existing ones are left
  alone) and registers its alias immediately, without needing a pod restart.
- Rotating the SYSDBA secret's password (the auto-generated one, or a user-provided
  `authentication.sysdba.secretRef`) pushes the new password to the live server automatically, so
  the secret and the running instance never drift apart.

Deleting an `Instance` relies on Kubernetes garbage collection of the objects Kubebird created for
it (the Secret, aliases ConfigMap, Service and StatefulSet are all owned by the `Instance`); the
operator itself just logs the deletion and reports `status.phase: Deleting` while that garbage
collection runs. The primary/shadow PVCs are **not** removed with it — Kubernetes retains a
StatefulSet's PVCs by default, so an `Instance`'s data survives its deletion. Delete the PVCs
yourself once you're sure you no longer need the data:
```bash
kubectl delete pvc -l kubebird.github.io/instance=<name>
```
