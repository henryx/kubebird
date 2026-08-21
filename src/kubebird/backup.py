"""Backup CR handlers: gbak inside the referenced Instance's pod, optionally
uploaded to S3 via boto3.

Kept as its own module (mirroring create.py/update.py/delete.py, one per
Instance lifecycle stage) since it drives its own CR's status/patch, rather
than folding into create.py/update.py, which already have their hands full
with the Instance lifecycle. The actual gbak/mkdir/rm/file-read pod-exec
primitives live in firebird.py instead, for the same reason
provision_databases does: pure pod-exec orchestration, no Backup-specific
status/patch handling of its own.
"""

from datetime import UTC, datetime
from typing import Any

import boto3
import kopf
from kubernetes import client
from kubernetes.client.exceptions import ApiException

from . import firebird, k8s

CONTAINER_NAME = "firebird"
INSTANCE_GROUP = "kubebird.github.io"
INSTANCE_VERSION = "v1"
INSTANCE_PLURAL = "instances"


def _timestamp() -> str:
    return datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")


def _get_instance(
    objects_api: client.CustomObjectsApi, *, namespace: str, instance_name: str
) -> kopf.RawBody:
    try:
        return objects_api.get_namespaced_custom_object(  # type: ignore[no-any-return]
            INSTANCE_GROUP, INSTANCE_VERSION, namespace, INSTANCE_PLURAL, instance_name
        )
    except ApiException as e:
        if e.status == 404:
            raise kopf.PermanentError(
                f"instanceRef {instance_name!r} does not exist."
            ) from e
        raise


def _select_databases(instance_spec: dict[str, Any], spec: kopf.Spec) -> list[str]:
    all_names = [db["name"] for db in instance_spec["databases"]]
    include = spec.get("include") or all_names
    unknown = sorted(set(include) - set(all_names))
    if unknown:
        raise kopf.PermanentError(
            f"include names not found in Instance's spec.databases: {unknown}"
        )
    exclude = set(spec.get("exclude") or [])
    selected = [name for name in include if name not in exclude]
    if not selected:
        raise kopf.PermanentError(
            "No databases left to back up once include/exclude are applied."
        )
    return selected


def _s3_client(
    core_api: client.CoreV1Api, *, namespace: str, s3_spec: dict[str, Any]
) -> Any:
    secret_ref = (s3_spec.get("credentials") or {}).get("ref")
    if not secret_ref:
        raise kopf.PermanentError(
            "spec.s3.credentials.ref is required when spec.type is 's3'."
        )
    secret = core_api.read_namespaced_secret(secret_ref, namespace)
    return boto3.client(
        "s3",
        endpoint_url=s3_spec["location"],
        aws_access_key_id=k8s.read_secret_value(secret, "accessKey"),
        aws_secret_access_key=k8s.read_secret_value(secret, "secretKey"),
    )


def _s3_key(s3_spec: dict[str, Any], backup_dir_name: str, filename: str) -> str:
    prefix = (s3_spec.get("path") or "/").strip("/")
    return (
        f"{prefix}/{backup_dir_name}/{filename}"
        if prefix
        else f"{backup_dir_name}/{filename}"
    )


@kopf.on.create(kind="Backup", version="v1", group="kubebird.github.io")
def backup_create_fn(
    spec: kopf.Spec,
    status: kopf.Status,
    name: str,
    namespace: str | None,
    logger: kopf.Logger,
    patch: kopf.Patch,
    **_: Any,
) -> None:
    if namespace is None:
        raise kopf.PermanentError("Backup is namespaced")

    try:
        _reconcile(spec, status, name, namespace, logger, patch)
    except Exception as exc:
        patch.status["error"] = str(exc)
        raise
    patch.status["error"] = ""


def _reconcile(
    spec: kopf.Spec,
    status: kopf.Status,
    name: str,
    namespace: str,
    logger: kopf.Logger,
    patch: kopf.Patch,
) -> None:
    core_api = client.CoreV1Api()
    objects_api = client.CustomObjectsApi()

    instance_name = spec["instanceRef"]
    logger.info(f"Backing up instance {instance_name!r} for Backup {name!r}.")
    patch.status["phase"] = "BackingUp"

    instance = _get_instance(
        objects_api, namespace=namespace, instance_name=instance_name
    )
    instance_spec = instance["spec"]

    if not (instance_spec.get("storage") or {}).get("backup"):
        raise kopf.PermanentError(
            f"Instance {instance_name!r} has no spec.storage.backup configured; "
            "a Backup needs the backup PVC mounted at k8s.BACKUP_MOUNT_PATH to stage into."
        )

    databases = _select_databases(instance_spec, spec)

    # Idempotent across handler retries: reuse a timestamp already recorded
    # by an earlier attempt (kopf persists patch.status even when a handler
    # ultimately raises) instead of generating -- and backing up into -- a
    # fresh directory on every retry.
    timestamp = status.get("timestamp") or _timestamp()
    patch.status["timestamp"] = timestamp
    backup_dir_name = f"{timestamp}-{name}"
    backup_dir = f"{k8s.BACKUP_MOUNT_PATH}/{backup_dir_name}"
    patch.status["path"] = backup_dir

    authentication = instance_spec.get("authentication") or {}
    sysdba_secret_ref = (authentication.get("sysdba") or {}).get("secretRef", "")
    # ensure_sysdba_secret only actually uses `body` to kopf.adopt() a
    # brand-new secret, which shouldn't happen here in practice (the
    # Instance's create_fn already provisioned it) -- wrapped in kopf.Body
    # regardless, since it's typed for that path.
    _secret_name, sysdba_password = k8s.ensure_sysdba_secret(
        core_api,
        namespace=namespace,
        name=instance_name,
        secret_ref=sysdba_secret_ref,
        body=kopf.Body(instance),
        logger=logger,
    )

    pod_name = f"{instance_name}-0"
    firebird.mkdir_p(
        core_api,
        namespace=namespace,
        pod_name=pod_name,
        container=CONTAINER_NAME,
        path=backup_dir,
        logger=logger,
    )

    backup_type = spec.get("type") or "local"
    s3_spec = spec.get("s3") or {}
    s3_client = (
        _s3_client(core_api, namespace=namespace, s3_spec=s3_spec)
        if backup_type == "s3"
        else None
    )

    # Recorded incrementally (not just once at the end) so that, if a later
    # database in this loop fails, this attempt's patch still remembers
    # every file already produced/uploaded before that -- otherwise a
    # subsequent Backup deletion would have no way to know about (and clean
    # up) an S3 object this same attempt already created.
    files: list[str] = []
    patch.status["files"] = files
    for db_name in databases:
        db_path = f"{k8s.DATA_MOUNT_PATH}/{db_name}"
        backup_filename = f"{db_name}.fbk"
        backup_path = f"{backup_dir}/{backup_filename}"
        logger.info(f"Backing up database {db_path!r} to {backup_path!r}.")
        firebird.create_backup(
            core_api,
            namespace=namespace,
            pod_name=pod_name,
            container=CONTAINER_NAME,
            sysdba_password=sysdba_password,
            database_path=db_path,
            backup_path=backup_path,
            logger=logger,
        )
        files.append(backup_filename)

        if s3_client is not None:
            content = firebird.read_file_base64(
                core_api,
                namespace=namespace,
                pod_name=pod_name,
                container=CONTAINER_NAME,
                path=backup_path,
                logger=logger,
            )
            key = _s3_key(s3_spec, backup_dir_name, backup_filename)
            logger.info(f"Uploading {backup_path!r} to s3://{s3_spec['bucket']}/{key}.")
            s3_client.put_object(Bucket=s3_spec["bucket"], Key=key, Body=content)

    patch.status["files"] = files
    patch.status["phase"] = "Ready"
    patch.status["message"] = "Backup completed."
    logger.info(f"Backup {name!r} completed successfully.")


@kopf.on.delete(kind="Backup", version="v1", group="kubebird.github.io")
def backup_delete_fn(
    spec: kopf.Spec,
    status: kopf.Status,
    name: str,
    namespace: str | None,
    logger: kopf.Logger,
    patch: kopf.Patch,
    **_: Any,
) -> None:
    if namespace is None:
        raise kopf.PermanentError("Backup is namespaced")

    try:
        _reconcile_delete(spec, status, name, namespace, logger, patch)
    except Exception as exc:
        patch.status["error"] = str(exc)
        raise
    patch.status["error"] = ""


def _reconcile_delete(
    spec: kopf.Spec,
    status: kopf.Status,
    name: str,
    namespace: str,
    logger: kopf.Logger,
    patch: kopf.Patch,
) -> None:
    logger.info(f"Removing backup {name!r}.")
    patch.status["phase"] = "Deleting"

    backup_path = status.get("path")
    if not backup_path:
        # create_fn never got far enough to record a path (e.g. it failed
        # before even creating the directory) -- nothing to remove.
        return

    core_api = client.CoreV1Api()
    instance_name = spec["instanceRef"]
    pod_name = f"{instance_name}-0"
    try:
        firebird.remove_path(
            core_api,
            namespace=namespace,
            pod_name=pod_name,
            container=CONTAINER_NAME,
            path=backup_path,
            logger=logger,
        )
    except ApiException as e:
        if e.status != 404:
            raise
        # The Instance (and therefore its pod) is already gone -- the backup
        # PVC survives Instance deletion (see storage.backup), but there's no
        # running container left to exec into and clean it up from.
        logger.warning(
            f"Pod {pod_name!r} not found while removing backup {name!r}; "
            "its Instance was likely already deleted."
        )

    if (spec.get("type") or "local") != "s3":
        return

    files = status.get("files") or []
    timestamp = status.get("timestamp")
    if not files or not timestamp:
        return

    s3_spec = spec.get("s3") or {}
    backup_dir_name = f"{timestamp}-{name}"
    s3_client = _s3_client(core_api, namespace=namespace, s3_spec=s3_spec)
    keys = [_s3_key(s3_spec, backup_dir_name, f) for f in files]
    logger.info(f"Removing {len(keys)} object(s) from s3://{s3_spec['bucket']}.")
    s3_client.delete_objects(
        Bucket=s3_spec["bucket"],
        Delete={"Objects": [{"Key": key} for key in keys]},
    )
