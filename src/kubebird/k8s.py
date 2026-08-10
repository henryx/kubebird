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


def read_user_credentials(
    core_api: client.CoreV1Api, *, namespace: str, secret_ref: str
) -> tuple[str, str]:
    secret = core_api.read_namespaced_secret(secret_ref, namespace)
    return read_secret_value(secret, "username"), read_secret_value(secret, "password")


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
    shadow_pvc_name: str | None = None,
) -> dict[str, Any]:
    labels = {INSTANCE_LABEL: name}
    volume_mounts = [{"name": "data", "mountPath": DATA_MOUNT_PATH}]
    volumes = [{"name": "data", "persistentVolumeClaim": {"claimName": pvc_name}}]
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
