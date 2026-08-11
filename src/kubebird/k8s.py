"""Kubernetes resource builders and idempotent creation helpers for Instance reconciliation."""

import base64
import secrets
from collections.abc import Callable
from typing import Any

import kopf
from kubernetes import client
from kubernetes.client.exceptions import ApiException

FIREBIRD_PORT = 3050
DATA_MOUNT_PATH = "/var/lib/firebird/data"
SHADOW_MOUNT_PATH = "/var/lib/firebird/shadow"
DATABASES_CONF_PATH = "/opt/firebird/databases.conf"
DATABASES_CONF_KEY = "databases.conf"

INSTANCE_LABEL = "kubebird.github.io/instance"
SYSDBA_ROLE_LABEL = "kubebird.github.io/role"
SYSDBA_ROLE_VALUE = "sysdba"


def generate_password(length: int = 32) -> str:
    # token_urlsafe's alphabet includes "-", which isql/gsec's argument
    # parsers can mistake for the start of another switch (e.g. "-password
    # -abc..." reads as two switches, not an option and its value).
    while True:
        password = secrets.token_urlsafe(length)
        if not password.startswith("-"):
            return password


def create_or_ignore(
    create_call: Callable[..., Any],
    namespace: str,
    body: dict[str, Any],
    logger: kopf.Logger,
) -> None:
    """Create the object, tolerating a 409 Conflict from a previous handler retry."""
    try:
        create_call(namespace=namespace, body=body)
    except ApiException as e:
        if e.status != 409:
            raise
        logger.debug(
            f"{body['kind']} {body['metadata']['name']!r} already exists, skipping creation."
        )


def read_secret_value(secret: client.V1Secret, key: str) -> str:
    if not secret.data or key not in secret.data:
        raise kopf.PermanentError(
            f"Secret {secret.metadata.name!r} has no {key!r} key."
        )
    return base64.b64decode(secret.data[key]).decode()


def ensure_sysdba_secret(
    core_api: client.CoreV1Api,
    *,
    namespace: str,
    name: str,
    secret_ref: str,
    body: kopf.Body,
    logger: kopf.Logger,
) -> tuple[str, str]:
    """Return (secret_name, password) for the SYSDBA user, creating a secret if needed."""
    if secret_ref:
        secret = core_api.read_namespaced_secret(secret_ref, namespace)
        _label_sysdba_secret(
            core_api,
            namespace=namespace,
            secret=secret,
            instance_name=name,
            logger=logger,
        )
        return secret_ref, read_secret_value(secret, "password")

    secret_name = f"{name}-sysdba"
    password = generate_password()
    secret_body: dict[str, Any] = {
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {
            "name": secret_name,
            "namespace": namespace,
            "labels": {INSTANCE_LABEL: name, SYSDBA_ROLE_LABEL: SYSDBA_ROLE_VALUE},
        },
        "stringData": {"username": "SYSDBA", "password": password},
    }
    kopf.adopt(secret_body, owner=body)
    try:
        core_api.create_namespaced_secret(namespace=namespace, body=secret_body)
    except ApiException as e:
        if e.status != 409:
            raise
        # A handler retry: the secret (and its actual live password) already
        # exists from a previous attempt -- reuse it instead of the freshly
        # generated value above, which was never applied anywhere.
        logger.debug(f"Secret {secret_name!r} already exists, reusing its password.")
        existing = core_api.read_namespaced_secret(secret_name, namespace)
        password = read_secret_value(existing, "password")
    return secret_name, password


def _label_sysdba_secret(
    core_api: client.CoreV1Api,
    *,
    namespace: str,
    secret: client.V1Secret,
    instance_name: str,
    logger: kopf.Logger,
) -> None:
    """Label a user-referenced SYSDBA secret so a rotation on it is noticed.

    We don't own this secret (no kopf.adopt()), just tag it with the same
    labels the auto-generated secret gets, so sysdba_secret_update_fn's watch
    (filtered by SYSDBA_ROLE_LABEL) also covers it. If the same secret is
    referenced by more than one Instance, INSTANCE_LABEL reflects whichever
    one last reconciled it.
    """
    labels = secret.metadata.labels or {}
    if (
        labels.get(SYSDBA_ROLE_LABEL) == SYSDBA_ROLE_VALUE
        and labels.get(INSTANCE_LABEL) == instance_name
    ):
        return
    core_api.patch_namespaced_secret(
        secret.metadata.name,
        namespace,
        {
            "metadata": {
                "labels": {
                    INSTANCE_LABEL: instance_name,
                    SYSDBA_ROLE_LABEL: SYSDBA_ROLE_VALUE,
                }
            }
        },
        _content_type="application/merge-patch+json",
    )
    logger.debug(
        f"Labeled secret {secret.metadata.name!r} for SYSDBA password-change watching."
    )


def security_db_filename(version: str) -> str:
    """The security database's own filename, which is version-specific
    (security3.fdb for Firebird 3, security4.fdb for 4, security5.fdb for 5)."""
    major = version.split(".", 1)[0]
    if not major.isdigit():
        raise kopf.PermanentError(f"Cannot determine major version from {version!r}.")
    return f"security{major}.fdb"


def render_databases_conf(databases: list[dict[str, Any]], version: str) -> str:
    """Render databases.conf content: one alias per spec.databases entry (using
    "alias" if set, otherwise the database's own "name", e.g. "instance.fdb"),
    pointing at its full path under DATA_MOUNT_PATH, plus the version-specific
    security.db alias that the image's own default databases.conf would
    otherwise provide -- since this file replaces that default wholesale,
    security.db must be replicated here or SYSDBA authentication itself breaks.

    The security.db entry must match the image's own default verbatim (path
    via the "$(dir_secDb)" macro, not a bare filename -- confirmed empirically:
    a bare filename resolves relative to whatever the entrypoint's current
    working directory happens to be at the time, not $(dir_secDb), and fails
    with "I/O error ... No such file or directory"; and RemoteAccess = false,
    the same setting the "always connect via localhost:" gotcha above depends
    on to keep security.db unreachable from outside the pod).
    """
    lines = [
        f"security.db = $(dir_secDb)/{security_db_filename(version)}",
        "{",
        "\tRemoteAccess = false",
        "\tDefaultDbCachePages = 50",
        "}",
    ]
    for database in databases:
        name = database["name"]
        alias = database.get("alias") or name
        lines.append(f"{alias} = {DATA_MOUNT_PATH}/{name}")
    return "\n".join(lines) + "\n"


def build_databases_conf_configmap(
    *,
    name: str,
    namespace: str,
    instance_name: str,
    databases: list[dict[str, Any]],
    version: str,
) -> dict[str, Any]:
    return {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "labels": {INSTANCE_LABEL: instance_name},
        },
        "data": {DATABASES_CONF_KEY: render_databases_conf(databases, version)},
    }


def build_pvc(
    *, pvc_name: str, namespace: str, instance_name: str, storage: dict[str, Any]
) -> dict[str, Any]:
    spec: dict[str, Any] = {
        "accessModes": ["ReadWriteOnce"],
        "resources": {"requests": {"storage": storage["size"]}},
    }
    if storage.get("class"):
        spec["storageClassName"] = storage["class"]
    return {
        "apiVersion": "v1",
        "kind": "PersistentVolumeClaim",
        "metadata": {
            "name": pvc_name,
            "namespace": namespace,
            "labels": {INSTANCE_LABEL: instance_name},
        },
        "spec": spec,
    }


def build_service(
    *, name: str, namespace: str, service_spec: dict[str, Any]
) -> dict[str, Any]:
    return {
        "apiVersion": "v1",
        "kind": "Service",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "labels": {INSTANCE_LABEL: name},
        },
        "spec": {
            "type": service_spec.get("type") or "ClusterIP",
            "selector": {INSTANCE_LABEL: name},
            "ports": [
                {"name": "firebird", "port": FIREBIRD_PORT, "targetPort": FIREBIRD_PORT}
            ],
        },
    }


def build_statefulset(
    *,
    name: str,
    namespace: str,
    image: str,
    version: str,
    pvc_name: str,
    sysdba_secret_name: str,
    databases_conf_configmap_name: str,
    shadow_pvc_name: str | None = None,
) -> dict[str, Any]:
    labels = {INSTANCE_LABEL: name}
    # ConfigMap volumes are always mounted read-only by kubelet -- unrelated to
    # and not overridable by subPath or a volumeMount's own readOnly setting --
    # so databases.conf can't live directly on one: firebird.write_databases_conf
    # execs a live rewrite whenever spec.databases/spec.version change, and that
    # would fail with "Read-only file system" against a ConfigMap-backed mount.
    # Instead the ConfigMap is only the seed: an initContainer copies it onto a
    # plain (writable) emptyDir on every pod (re)start, and the main container
    # mounts *that* over DATABASES_CONF_PATH via subPath -- subPath itself
    # doesn't force read-only, only the ConfigMap volume type does.
    databases_conf_configmap_mount = "/var/run/kubebird/databases-conf-configmap"
    databases_conf_writable_mount = "/var/run/kubebird/databases-conf-writable"
    volume_mounts = [
        {"name": "data", "mountPath": DATA_MOUNT_PATH},
        {
            "name": "databases-conf-writable",
            "mountPath": DATABASES_CONF_PATH,
            "subPath": DATABASES_CONF_KEY,
        },
    ]
    volumes = [
        {"name": "data", "persistentVolumeClaim": {"claimName": pvc_name}},
        {
            "name": "databases-conf-configmap",
            "configMap": {"name": databases_conf_configmap_name},
        },
        {"name": "databases-conf-writable", "emptyDir": {}},
    ]
    if shadow_pvc_name:
        volume_mounts.append({"name": "shadow", "mountPath": SHADOW_MOUNT_PATH})
        volumes.append(
            {"name": "shadow", "persistentVolumeClaim": {"claimName": shadow_pvc_name}}
        )
    return {
        "apiVersion": "apps/v1",
        "kind": "StatefulSet",
        "metadata": {"name": name, "namespace": namespace, "labels": labels},
        "spec": {
            "serviceName": name,
            "replicas": 1,
            "selector": {"matchLabels": labels},
            "template": {
                "metadata": {"labels": labels},
                "spec": {
                    "initContainers": [
                        {
                            "name": "databases-conf-init",
                            "image": f"{image}:{version}",
                            "command": [
                                "/bin/sh",
                                "-c",
                                (
                                    f"cp {databases_conf_configmap_mount}/{DATABASES_CONF_KEY} "
                                    f"{databases_conf_writable_mount}/{DATABASES_CONF_KEY}"
                                ),
                            ],
                            "volumeMounts": [
                                {
                                    "name": "databases-conf-configmap",
                                    "mountPath": databases_conf_configmap_mount,
                                    "readOnly": True,
                                },
                                {
                                    "name": "databases-conf-writable",
                                    "mountPath": databases_conf_writable_mount,
                                },
                            ],
                        }
                    ],
                    "containers": [
                        {
                            "name": "firebird",
                            "image": f"{image}:{version}",
                            "ports": [{"containerPort": FIREBIRD_PORT}],
                            "env": [
                                {
                                    "name": "FIREBIRD_ROOT_PASSWORD",
                                    "valueFrom": {
                                        "secretKeyRef": {
                                            "name": sysdba_secret_name,
                                            "key": "password",
                                        }
                                    },
                                }
                            ],
                            "volumeMounts": volume_mounts,
                        }
                    ],
                    "volumes": volumes,
                },
            },
        },
    }
