from typing import Any

import kopf
from kubernetes import client

from . import firebird, k8s

CONTAINER_NAME = "firebird"


@kopf.on.create(kind="Instance", version="v1", group="kubebird.github.io")
def create_fn(
    spec: kopf.Spec,
    name: str,
    namespace: str | None,
    logger: kopf.Logger,
    body: kopf.Body,
    patch: kopf.Patch,
    **_: Any,
) -> None:
    if namespace is None:
        raise kopf.PermanentError("Instance is namespaced")

    try:
        _reconcile(spec, name, namespace, logger, body, patch)
    except Exception as exc:
        patch.status["error"] = str(exc)
        raise
    patch.status["error"] = ""


def _reconcile(
    spec: kopf.Spec,
    name: str,
    namespace: str,
    logger: kopf.Logger,
    body: kopf.Body,
    patch: kopf.Patch,
) -> None:
    core_api = client.CoreV1Api()
    apps_api = client.AppsV1Api()

    logger.info(f"Provisioning instance {name!r}.")
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

    databases_conf_configmap_body = k8s.build_databases_conf_configmap(
        name=f"{name}-databases-conf",
        namespace=namespace,
        instance_name=name,
        databases=spec["databases"],
        version=spec["version"],
    )
    kopf.adopt(databases_conf_configmap_body, owner=body)
    k8s.create_or_ignore(
        core_api.create_namespaced_config_map,
        namespace,
        databases_conf_configmap_body,
        logger,
    )

    pvc_body = k8s.build_pvc(
        pvc_name=f"{name}-data",
        namespace=namespace,
        instance_name=name,
        storage=storage["primary"],
    )
    kopf.adopt(pvc_body, owner=body)
    k8s.create_or_ignore(
        core_api.create_namespaced_persistent_volume_claim, namespace, pvc_body, logger
    )

    shadow_pvc_name = None
    if shadow_storage:
        shadow_pvc_body = k8s.build_pvc(
            pvc_name=f"{name}-shadow",
            namespace=namespace,
            instance_name=name,
            storage=shadow_storage,
        )
        kopf.adopt(shadow_pvc_body, owner=body)
        k8s.create_or_ignore(
            core_api.create_namespaced_persistent_volume_claim,
            namespace,
            shadow_pvc_body,
            logger,
        )
        shadow_pvc_name = shadow_pvc_body["metadata"]["name"]

    backup_storage = storage.get("backup")
    backup_pvc_name = None
    if backup_storage:
        backup_pvc_body = k8s.build_pvc(
            pvc_name=f"{name}-backup",
            namespace=namespace,
            instance_name=name,
            storage=backup_storage,
        )
        # Deliberately NOT kopf.adopt()-ed: unlike the primary/shadow PVCs,
        # this one must survive Instance deletion so backup data isn't lost
        # along with it.
        k8s.create_or_ignore(
            core_api.create_namespaced_persistent_volume_claim,
            namespace,
            backup_pvc_body,
            logger,
        )
        backup_pvc_name = backup_pvc_body["metadata"]["name"]

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
        backup_pvc_name=backup_pvc_name,
        sysdba_secret_name=secret_name,
        databases_conf_configmap_name=databases_conf_configmap_body["metadata"]["name"],
    )
    kopf.adopt(statefulset_body, owner=body)
    k8s.create_or_ignore(
        apps_api.create_namespaced_stateful_set, namespace, statefulset_body, logger
    )

    patch.status["phase"] = "WaitingForPod"
    pod_name = f"{name}-0"
    firebird.wait_for_pod_ready(
        core_api, namespace=namespace, pod_name=pod_name, logger=logger
    )
    firebird.wait_for_sysdba_ready(
        core_api,
        namespace=namespace,
        pod_name=pod_name,
        container=CONTAINER_NAME,
        sysdba_password=sysdba_password,
        logger=logger,
    )

    patch.status["phase"] = "ProvisioningDatabases"
    firebird.provision_databases(
        core_api,
        namespace=namespace,
        pod_name=pod_name,
        container=CONTAINER_NAME,
        sysdba_password=sysdba_password,
        databases=spec["databases"],
        shadow_storage=shadow_storage,
        logger=logger,
    )

    patch.status["phase"] = "Ready"
    patch.status["message"] = "Instance provisioned."
    logger.info(f"Instance {name!r} provisioned successfully.")
