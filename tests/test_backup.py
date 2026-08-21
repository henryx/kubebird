import time
from pathlib import Path

import boto3
import yaml
from botocore.exceptions import ClientError
from kopf.testing import KopfRunner
from kubernetes import client, config
from kubernetes.client.exceptions import ApiException
from kubernetes.stream import stream
from test_create import CR_PATH, CRD_PATH, _ensure_crd_established, _wait_ready
from test_delete import _wait_gone
from testcontainers.core.container import DockerContainer

from kubebird.backup import CONTAINER_NAME
from kubebird.k8s import BACKUP_MOUNT_PATH

DEPLOY_DIR = Path(__file__).resolve().parent.parent / "deploy"
BACKUP_CRD_PATH = DEPLOY_DIR / "backup-crd.yaml"
BACKUP_CR_PATH = DEPLOY_DIR / "backup-cr.yaml"

S3_IMAGE = "pgsty/silo:RELEASE.2026-08-06T00-00-00Z"
S3_ACCESS_KEY = "kubebirdtest"
S3_SECRET_KEY = "kubebirdtestsecret"
S3_BUCKET = "kubebird-backups"


def _wait_backup_ready(
    api: client.CustomObjectsApi,
    *,
    group: str,
    version: str,
    namespace: str,
    plural: str,
    name: str,
    timeout: float = 180.0,
) -> dict:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        obj = api.get_namespaced_custom_object(group, version, namespace, plural, name)
        status = obj.get("status") or {}
        if status.get("phase") == "Ready":
            return obj
        if status.get("error"):
            # Fail fast on a real, reported error rather than waiting out the
            # whole timeout for something that will never reach Ready.
            raise AssertionError(
                f"Backup {name!r} reported an error: {status['error']}"
            )
        time.sleep(2)
    raise TimeoutError(f"Backup {name!r} did not reach phase Ready in time")


def _assert_path_exists(
    core_api: client.CoreV1Api, *, namespace: str, pod_name: str, path: str
) -> None:
    output = stream(
        core_api.connect_get_namespaced_pod_exec,
        pod_name,
        namespace,
        container=CONTAINER_NAME,
        command=["/bin/sh", "-c", f"test -e '{path}' && echo EXISTS"],
        stderr=True,
        stdin=False,
        stdout=True,
        tty=False,
    )
    assert "EXISTS" in output, f"{path!r} does not exist in pod {pod_name!r}: {output}"


def _assert_path_gone(
    core_api: client.CoreV1Api, *, namespace: str, pod_name: str, path: str
) -> None:
    output = stream(
        core_api.connect_get_namespaced_pod_exec,
        pod_name,
        namespace,
        container=CONTAINER_NAME,
        command=["/bin/sh", "-c", f"test -e '{path}' && echo EXISTS"],
        stderr=True,
        stdin=False,
        stdout=True,
        tty=False,
    )
    assert "EXISTS" not in output, (
        f"{path!r} still exists in pod {pod_name!r}: {output}"
    )


def test_backup_instance_local(kubeconfig: Path) -> None:
    """A "local" Backup writes a gbak file onto the Instance's storage.backup
    PVC (under a <timestamp>-<name> directory), and removes it again when the
    Backup CR itself is deleted."""
    config.load_kube_config(config_file=str(kubeconfig))

    crd_body = yaml.safe_load(CRD_PATH.read_text())
    cr_body = yaml.safe_load(CR_PATH.read_text())
    cr_body["metadata"]["name"] = "test-backup-local"

    group, version = cr_body["apiVersion"].split("/")
    namespace = "default"
    instance_name = cr_body["metadata"]["name"]

    backup_crd_body = yaml.safe_load(BACKUP_CRD_PATH.read_text())
    backup_cr_body = yaml.safe_load(BACKUP_CR_PATH.read_text())
    backup_cr_body["metadata"]["name"] = "test-backup-local"
    backup_cr_body["spec"]["instanceRef"] = instance_name
    del backup_cr_body["spec"]["s3"]
    backup_plural = backup_crd_body["spec"]["names"]["plural"]
    backup_name = backup_cr_body["metadata"]["name"]

    extensions_api = client.ApiextensionsV1Api()
    _ensure_crd_established(extensions_api, crd_body)
    _ensure_crd_established(extensions_api, backup_crd_body)

    objects_api = client.CustomObjectsApi()

    with KopfRunner(
        [
            "run",
            "-n",
            namespace,
            "--verbose",
            "-m",
            "kubebird.create",
            "-m",
            "kubebird.backup",
        ]
    ) as runner:
        objects_api.create_namespaced_custom_object(
            group, version, namespace, "instances", cr_body
        )
        instance = _wait_ready(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural="instances",
            name=instance_name,
        )
        assert instance["status"]["phase"] == "Ready"

        objects_api.create_namespaced_custom_object(
            group, version, namespace, backup_plural, backup_cr_body
        )
        backup = _wait_backup_ready(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=backup_plural,
            name=backup_name,
        )
        backup_path = backup["status"]["path"]
        assert backup["status"]["files"] == ["instance.fdb.fbk"]
        assert backup_path.startswith(BACKUP_MOUNT_PATH)

        core_api = client.CoreV1Api()
        pod_name = f"{instance_name}-0"
        _assert_path_exists(
            core_api,
            namespace=namespace,
            pod_name=pod_name,
            path=f"{backup_path}/instance.fdb.fbk",
        )

        objects_api.delete_namespaced_custom_object(
            group, version, namespace, backup_plural, backup_name
        )
        _wait_gone(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=backup_plural,
            name=backup_name,
        )
        _assert_path_gone(
            core_api, namespace=namespace, pod_name=pod_name, path=backup_path
        )

        objects_api.delete_namespaced_custom_object(
            group, version, namespace, "instances", instance_name
        )
        time.sleep(1)

    assert runner.exit_code == 0
    assert runner.exception is None
    assert "Handler 'backup_create_fn' succeeded." in runner.output
    assert "Handler 'backup_delete_fn' succeeded." in runner.output


def _s3_client(*, host: str, port: int):
    return boto3.client(
        "s3",
        endpoint_url=f"http://{host}:{port}",
        aws_access_key_id=S3_ACCESS_KEY,
        aws_secret_access_key=S3_SECRET_KEY,
    )


def test_backup_instance_s3(kubeconfig: Path) -> None:
    """A "s3" Backup does everything the "local" test above does, plus
    uploads the same gbak file to an S3-compatible endpoint (here, a
    throwaway pgsty/silo -- a maintained, API-compatible MinIO fork --
    container), and removes the uploaded object again on Backup deletion."""
    config.load_kube_config(config_file=str(kubeconfig))

    s3_container = (
        DockerContainer(S3_IMAGE)
        .with_env("MINIO_ROOT_USER", S3_ACCESS_KEY)
        .with_env("MINIO_ROOT_PASSWORD", S3_SECRET_KEY)
        .with_exposed_ports(9000)
        .with_command("server /data --console-address :9001")
    )
    # DockerContainer.__enter__ calls start() itself -- with_env/with_command
    # etc. must be configured on the container before that, not after.
    with s3_container:
        s3_host = s3_container.get_container_host_ip()
        s3_port = int(s3_container.get_exposed_port(9000))

        s3_client = _s3_client(host=s3_host, port=s3_port)
        deadline = time.monotonic() + 60.0
        while True:
            try:
                s3_client.list_buckets()
                break
            except Exception:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(1)
        s3_client.create_bucket(Bucket=S3_BUCKET)

        crd_body = yaml.safe_load(CRD_PATH.read_text())
        cr_body = yaml.safe_load(CR_PATH.read_text())
        cr_body["metadata"]["name"] = "test-backup-s3"

        group, version = cr_body["apiVersion"].split("/")
        namespace = "default"
        instance_name = cr_body["metadata"]["name"]

        backup_crd_body = yaml.safe_load(BACKUP_CRD_PATH.read_text())
        backup_cr_body = yaml.safe_load(BACKUP_CR_PATH.read_text())
        backup_cr_body["metadata"]["name"] = "test-backup-s3"
        backup_cr_body["spec"]["instanceRef"] = instance_name
        backup_cr_body["spec"]["type"] = "s3"
        backup_cr_body["spec"]["s3"] = {
            "location": f"http://{s3_host}:{s3_port}",
            "credentials": {"ref": "s3-creds"},
            "bucket": S3_BUCKET,
            "path": "/",
        }
        backup_plural = backup_crd_body["spec"]["names"]["plural"]
        backup_name = backup_cr_body["metadata"]["name"]

        extensions_api = client.ApiextensionsV1Api()
        _ensure_crd_established(extensions_api, crd_body)
        _ensure_crd_established(extensions_api, backup_crd_body)

        core_api = client.CoreV1Api()
        try:
            core_api.create_namespaced_secret(
                namespace,
                {
                    "apiVersion": "v1",
                    "kind": "Secret",
                    "metadata": {"name": "s3-creds"},
                    "stringData": {
                        "accessKey": S3_ACCESS_KEY,
                        "secretKey": S3_SECRET_KEY,
                    },
                },
            )
        except ApiException as e:
            if e.status != 409:
                raise

        objects_api = client.CustomObjectsApi()

        with KopfRunner(
            [
                "run",
                "-n",
                namespace,
                "--verbose",
                "-m",
                "kubebird.create",
                "-m",
                "kubebird.backup",
            ]
        ) as runner:
            objects_api.create_namespaced_custom_object(
                group, version, namespace, "instances", cr_body
            )
            instance = _wait_ready(
                objects_api,
                group=group,
                version=version,
                namespace=namespace,
                plural="instances",
                name=instance_name,
            )
            assert instance["status"]["phase"] == "Ready"

            objects_api.create_namespaced_custom_object(
                group, version, namespace, backup_plural, backup_cr_body
            )
            backup = _wait_backup_ready(
                objects_api,
                group=group,
                version=version,
                namespace=namespace,
                plural=backup_plural,
                name=backup_name,
            )
            timestamp = backup["status"]["timestamp"]
            key = f"{timestamp}-{backup_name}/instance.fdb.fbk"

            head = s3_client.head_object(Bucket=S3_BUCKET, Key=key)
            assert head["ContentLength"] > 0

            objects_api.delete_namespaced_custom_object(
                group, version, namespace, backup_plural, backup_name
            )
            _wait_gone(
                objects_api,
                group=group,
                version=version,
                namespace=namespace,
                plural=backup_plural,
                name=backup_name,
            )

            try:
                s3_client.head_object(Bucket=S3_BUCKET, Key=key)
                raise AssertionError(
                    f"S3 object {key!r} was not removed on Backup deletion"
                )
            except ClientError as e:
                assert e.response["Error"]["Code"] in ("404", "NoSuchKey")

            objects_api.delete_namespaced_custom_object(
                group, version, namespace, "instances", instance_name
            )
            time.sleep(1)

        assert runner.exit_code == 0
        assert runner.exception is None
