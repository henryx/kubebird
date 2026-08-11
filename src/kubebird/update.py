import base64
from typing import Any

import kopf
from kubernetes import client

from . import firebird, k8s
from .create import CONTAINER_NAME


@kopf.on.update(kind="Instance", version="v1", group="kubebird.github.io")
def update_fn(
    spec: kopf.Spec,
    old: kopf.BodyEssence | Any | None,
    name: str,
    namespace: str | None,
    logger: kopf.Logger,
    body: kopf.Body,
    patch: kopf.Patch,
    **_: Any,
) -> None:
    if namespace is None:
        raise kopf.PermanentError("Instance is namespaced")

    core_api = client.CoreV1Api()
    apps_api = client.AppsV1Api()

    patch.status["phase"] = "Updating"

    authentication = spec.get("authentication") or {}
    sysdba_secret_ref = (authentication.get("sysdba") or {}).get("secretRef", "")
    _secret_name, sysdba_password = k8s.ensure_sysdba_secret(
        core_api,
        namespace=namespace,
        name=name,
        secret_ref=sysdba_secret_ref,
        body=body,
        logger=logger,
    )

    # kubernetes' generated clients default a dict-bodied PATCH to
    # application/json-patch+json (which requires a list of operations, not
    # a merge dict) unless the content type is forced explicitly here.
    service_type = (spec.get("service") or {}).get("type") or "ClusterIP"
    core_api.patch_namespaced_service(
        name,
        namespace,
        {"spec": {"type": service_type}},
        _content_type="application/merge-patch+json",
    )

    pod_name = f"{name}-0"
    apps_api.patch_namespaced_stateful_set(
        name,
        namespace,
        {
            "spec": {
                "template": {
                    "spec": {
                        "containers": [
                            {
                                "name": CONTAINER_NAME,
                                "image": f"{spec['image']}:{spec['version']}",
                            }
                        ]
                    }
                }
            }
        },
        _content_type="application/strategic-merge-patch+json",
    )

    patch.status["phase"] = "WaitingForPod"
    firebird.wait_for_pod_ready(core_api, namespace=namespace, pod_name=pod_name)
    firebird.wait_for_sysdba_ready(
        core_api,
        namespace=namespace,
        pod_name=pod_name,
        container=CONTAINER_NAME,
        sysdba_password=sysdba_password,
    )

    shadow_storage = spec["storage"].get("shadow")
    old_database_names = {
        db["name"] for db in ((old or {}).get("spec") or {}).get("databases") or []
    }
    new_databases = [
        database
        for database in spec["databases"]
        if database["name"] not in old_database_names
    ]

    patch.status["phase"] = "ProvisioningDatabases"
    for database in new_databases:
        db_path = f"{k8s.DATA_MOUNT_PATH}/{database['name']}"
        logger.info(f"Creating database {db_path!r}.")
        firebird.create_database(
            core_api,
            namespace=namespace,
            pod_name=pod_name,
            container=CONTAINER_NAME,
            sysdba_password=sysdba_password,
            path=db_path,
            page_size=database.get("pageSize", 8192),
            charset=database.get("charset", "UTF8"),
            collation=database.get("collation", "UTF8"),
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

    patch.status["phase"] = "Ready"
    patch.status["message"] = "Instance updated."


@kopf.on.update(
    kind="Secret",
    version="v1",
    group="",
    labels={k8s.SYSDBA_ROLE_LABEL: k8s.SYSDBA_ROLE_VALUE},
)
def sysdba_secret_update_fn(
    old: kopf.BodyEssence | Any | None,
    new: kopf.BodyEssence | Any | None,
    meta: kopf.Meta,
    namespace: str | None,
    logger: kopf.Logger,
    **_: Any,
) -> None:
    if namespace is None:
        raise kopf.PermanentError("the SYSDBA Secret is namespaced")

    # "data" is a Secret-specific field, outside BodyEssence's generic
    # metadata/spec/status schema, so its .get() falls back to `object`.
    old_essence: Any = old or {}
    new_essence: Any = new or {}
    old_password_b64 = (old_essence.get("data") or {}).get("password")
    new_password_b64 = (new_essence.get("data") or {}).get("password")
    if not old_password_b64 or not new_password_b64:
        return  # nothing to compare against yet (e.g. secret just created)
    if old_password_b64 == new_password_b64:
        return  # some other field on the secret changed, not the password

    instance_name = meta["labels"][k8s.INSTANCE_LABEL]
    old_password = base64.b64decode(old_password_b64).decode()
    new_password = base64.b64decode(new_password_b64).decode()

    logger.info(
        f"SYSDBA secret changed for instance {instance_name!r}; "
        "updating the live password via gsec."
    )
    firebird.change_sysdba_password(
        client.CoreV1Api(),
        namespace=namespace,
        pod_name=f"{instance_name}-0",
        container=CONTAINER_NAME,
        old_password=old_password,
        new_password=new_password,
        logger=logger,
    )
