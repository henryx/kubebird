from typing import Any

import kopf
from kubernetes import client

from . import firebird, k8s

CONTAINER_NAME = "firebird"


@kopf.on.create(kind="Instance", version="v1", group="kubebird.github.io")
def create_fn(
    spec: kopf.Spec,
    name: str,
    namespace: str,
    logger: kopf.Logger,
    body: kopf.Body,
    patch: kopf.Patch,
    **_: Any,
) -> None:
    core_api = client.CoreV1Api()
    apps_api = client.AppsV1Api()

    patch.status["phase"] = "Provisioning"

    authentication = spec.get("authentication") or {}
    sysdba_secret_ref = (authentication.get("sysdba") or {}).get("secretRef", "")
    secret_name, sysdba_password = k8s.ensure_sysdba_secret(
        core_api,
        namespace=namespace,
        name=name,
        secret_ref=sysdba_secret_ref,
        body=body,
        logger=logger,
    )

    storage = spec["storage"]
    shadow_storage = storage.get("shadow")

    pvc_body = k8s.build_pvc(
        pvc_name=f"{name}-data", namespace=namespace, storage=storage["primary"]
    )
    kopf.adopt(pvc_body, owner=body)
    k8s.create_or_ignore(
        core_api.create_namespaced_persistent_volume_claim, namespace, pvc_body, logger
    )

    shadow_pvc_name = None
    if shadow_storage:
        shadow_pvc_body = k8s.build_pvc(
            pvc_name=f"{name}-shadow", namespace=namespace, storage=shadow_storage
        )
        kopf.adopt(shadow_pvc_body, owner=body)
        k8s.create_or_ignore(
            core_api.create_namespaced_persistent_volume_claim,
            namespace,
            shadow_pvc_body,
            logger,
        )
        shadow_pvc_name = shadow_pvc_body["metadata"]["name"]

    service_body = k8s.build_service(
        name=name, namespace=namespace, service_spec=spec.get("service") or {}
    )
    kopf.adopt(service_body, owner=body)
    k8s.create_or_ignore(
        core_api.create_namespaced_service, namespace, service_body, logger
    )

    statefulset_body = k8s.build_statefulset(
        name=name,
        namespace=namespace,
        image=spec["image"],
        version=spec["version"],
        pvc_name=pvc_body["metadata"]["name"],
        shadow_pvc_name=shadow_pvc_name,
        sysdba_secret_name=secret_name,
    )
    kopf.adopt(statefulset_body, owner=body)
    k8s.create_or_ignore(
        apps_api.create_namespaced_stateful_set, namespace, statefulset_body, logger
    )

    patch.status["phase"] = "WaitingForPod"
    pod_name = f"{name}-0"
    firebird.wait_for_pod_ready(core_api, namespace=namespace, pod_name=pod_name)
    firebird.wait_for_sysdba_ready(
        core_api,
        namespace=namespace,
        pod_name=pod_name,
        container=CONTAINER_NAME,
        sysdba_password=sysdba_password,
    )

    patch.status["phase"] = "ProvisioningDatabases"
    for database in spec["databases"]:
        db_path = f"{k8s.DATA_MOUNT_PATH}/{database['name']}"
        logger.info(f"Creating database {db_path!r}.")
        firebird.create_database(
            core_api,
            namespace=namespace,
            pod_name=pod_name,
            container=CONTAINER_NAME,
            sysdba_password=sysdba_password,
            path=db_path,
            logger=logger,
        )
        if database.get("shadow"):
            if shadow_storage is None:
                raise kopf.PermanentError(
                    f"database {database['name']!r} has shadow: true but "
                    "spec.storage.shadow is not configured."
                )
            shadow_path = f"{k8s.SHADOW_MOUNT_PATH}/{database['name']}.shadow"
            logger.info(f"Creating shadow for database {db_path!r}.")
            firebird.create_shadow(
                core_api,
                namespace=namespace,
                pod_name=pod_name,
                container=CONTAINER_NAME,
                sysdba_password=sysdba_password,
                database_path=db_path,
                shadow_path=shadow_path,
                logger=logger,
            )

    user_secret_ref = (authentication.get("user") or {}).get("secretRef", "")
    if user_secret_ref:
        username, password = k8s.read_user_credentials(
            core_api, namespace=namespace, secret_ref=user_secret_ref
        )
        first_db_path = f"{k8s.DATA_MOUNT_PATH}/{spec['databases'][0]['name']}"
        logger.info(f"Creating user {username!r}.")
        firebird.create_user(
            core_api,
            namespace=namespace,
            pod_name=pod_name,
            container=CONTAINER_NAME,
            sysdba_password=sysdba_password,
            database_path=first_db_path,
            username=username,
            password=password,
            logger=logger,
        )

    patch.status["phase"] = "Ready"
    patch.status["message"] = "Instance provisioned."
