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


def generate_password(length: int = 32) -> str:
    return secrets.token_urlsafe(length)


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
        return secret_ref, read_secret_value(secret, "password")

    secret_name = f"{name}-sysdba"
    password = generate_password()
    secret_body: dict[str, Any] = {
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {"name": secret_name, "namespace": namespace},
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


def read_user_credentials(
    core_api: client.CoreV1Api, *, namespace: str, secret_ref: str
) -> tuple[str, str]:
    secret = core_api.read_namespaced_secret(secret_ref, namespace)
    return read_secret_value(secret, "username"), read_secret_value(secret, "password")


def build_pvc(*, name: str, namespace: str, storage: dict[str, Any]) -> dict[str, Any]:
    spec: dict[str, Any] = {
        "accessModes": ["ReadWriteOnce"],
        "resources": {"requests": {"storage": storage["size"]}},
    }
    if storage.get("class"):
        spec["storageClassName"] = storage["class"]
    return {
        "apiVersion": "v1",
        "kind": "PersistentVolumeClaim",
        "metadata": {"name": f"{name}-data", "namespace": namespace},
        "spec": spec,
    }


def build_service(
    *, name: str, namespace: str, service_spec: dict[str, Any]
) -> dict[str, Any]:
    return {
        "apiVersion": "v1",
        "kind": "Service",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {
            "type": service_spec.get("type") or "ClusterIP",
            "selector": {"kubebird.github.io/instance": name},
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
) -> dict[str, Any]:
    labels = {"kubebird.github.io/instance": name}
    return {
        "apiVersion": "apps/v1",
        "kind": "StatefulSet",
        "metadata": {"name": name, "namespace": namespace},
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
                            "volumeMounts": [
                                {"name": "data", "mountPath": DATA_MOUNT_PATH}
                            ],
                        }
                    ],
                    "volumes": [
                        {
                            "name": "data",
                            "persistentVolumeClaim": {"claimName": pvc_name},
                        }
                    ],
                },
            },
        },
    }
