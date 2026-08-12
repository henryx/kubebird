import base64
import logging
import time
from pathlib import Path

import yaml
from kopf.testing import KopfRunner
from kubernetes import client, config
from test_create import (
    CR_PATH,
    CRD_PATH,
    _assert_database_file_exists,
    _ensure_crd_established,
    _read_databases_conf,
    _wait_ready,
)

from kubebird import firebird
from kubebird.k8s import DATA_MOUNT_PATH


def _runner_args(namespace: str) -> list[str]:
    return [
        "run",
        "-n",
        namespace,
        "--verbose",
        "-m",
        "kubebird.create",
        "-m",
        "kubebird.update",
    ]


def _wait_status_message(
    api: client.CustomObjectsApi,
    *,
    group: str,
    version: str,
    namespace: str,
    plural: str,
    name: str,
    message: str,
    timeout: float = 420.0,
) -> dict:
    """Wait for status.message to become `message`.

    update_fn's status starts out already at phase "Ready" (left over from
    create_fn), so polling for phase=="Ready" alone right after a patch would
    return immediately -- before update_fn has actually run. Waiting for its
    distinct completion message instead makes sure the update really happened.
    """
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        obj = api.get_namespaced_custom_object(group, version, namespace, plural, name)
        status = obj.get("status") or {}
        if status.get("message") == message and status.get("phase") == "Ready":
            return obj
        time.sleep(2)
    raise TimeoutError(f"Instance {name!r} did not reach message {message!r} in time")


def test_update_instance_service_type(kubeconfig: Path) -> None:
    """update_fn is expected to reconcile the Service when spec.service.type changes."""
    config.load_kube_config(config_file=str(kubeconfig))

    crd_body = yaml.safe_load(CRD_PATH.read_text())
    cr_body = yaml.safe_load(CR_PATH.read_text())
    cr_body["metadata"]["name"] = "test-update-service"

    group, version = cr_body["apiVersion"].split("/")
    plural = crd_body["spec"]["names"]["plural"]
    namespace = "default"
    name = cr_body["metadata"]["name"]

    extensions_api = client.ApiextensionsV1Api()
    _ensure_crd_established(extensions_api, crd_body)

    objects_api = client.CustomObjectsApi()
    core_api = client.CoreV1Api()

    with KopfRunner(_runner_args(namespace)) as runner:
        objects_api.create_namespaced_custom_object(
            group, version, namespace, plural, cr_body
        )
        _wait_ready(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
        )

        objects_api.patch_namespaced_custom_object(
            group,
            version,
            namespace,
            plural,
            name,
            {"spec": {"service": {"type": "NodePort"}}},
        )
        _wait_status_message(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
            message="Instance updated.",
        )

        service = core_api.read_namespaced_service(name, namespace)
        assert service.spec.type == "NodePort"

        objects_api.delete_namespaced_custom_object(
            group, version, namespace, plural, name
        )
        time.sleep(1)

    assert runner.exit_code == 0
    assert runner.exception is None
    assert "Handler 'update_fn' succeeded." in runner.output


def test_update_instance_version(kubeconfig: Path) -> None:
    """update_fn is expected to roll the StatefulSet's image when spec.version changes."""
    config.load_kube_config(config_file=str(kubeconfig))

    crd_body = yaml.safe_load(CRD_PATH.read_text())
    cr_body = yaml.safe_load(CR_PATH.read_text())
    cr_body["metadata"]["name"] = "test-update-version"

    group, version = cr_body["apiVersion"].split("/")
    plural = crd_body["spec"]["names"]["plural"]
    namespace = "default"
    name = cr_body["metadata"]["name"]
    image = cr_body["spec"]["image"]
    new_version = "3.0.13"  # one patch release older than deploy/cr.yaml's 3.0.14

    extensions_api = client.ApiextensionsV1Api()
    _ensure_crd_established(extensions_api, crd_body)

    objects_api = client.CustomObjectsApi()
    apps_api = client.AppsV1Api()

    with KopfRunner(_runner_args(namespace)) as runner:
        objects_api.create_namespaced_custom_object(
            group, version, namespace, plural, cr_body
        )
        _wait_ready(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
        )

        objects_api.patch_namespaced_custom_object(
            group, version, namespace, plural, name, {"spec": {"version": new_version}}
        )
        # update_fn itself waits for the rolled-out pod to become Ready and
        # SYSDBA-live before reporting completion, so by the time the status
        # message flips, the rollout has genuinely finished.
        _wait_status_message(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
            message="Instance updated.",
        )

        statefulset = apps_api.read_namespaced_stateful_set(name, namespace)
        assert statefulset.spec.template.spec.containers[0].image == (
            f"{image}:{new_version}"
        )

        objects_api.delete_namespaced_custom_object(
            group, version, namespace, plural, name
        )
        time.sleep(1)

    assert runner.exit_code == 0
    assert runner.exception is None
    assert "Handler 'update_fn' succeeded." in runner.output


def test_update_sysdba_secret_password_autogenerated(kubeconfig: Path) -> None:
    """sysdba_secret_update_fn is expected to push a rotated SYSDBA secret's
    password to the live server via `gsec`, for the auto-generated secret."""
    config.load_kube_config(config_file=str(kubeconfig))

    crd_body = yaml.safe_load(CRD_PATH.read_text())
    cr_body = yaml.safe_load(CR_PATH.read_text())
    cr_body["metadata"]["name"] = "test-update-sysdba"

    group, version = cr_body["apiVersion"].split("/")
    plural = crd_body["spec"]["names"]["plural"]
    namespace = "default"
    name = cr_body["metadata"]["name"]
    secret_name = f"{name}-sysdba"

    extensions_api = client.ApiextensionsV1Api()
    _ensure_crd_established(extensions_api, crd_body)

    objects_api = client.CustomObjectsApi()
    core_api = client.CoreV1Api()

    with KopfRunner(_runner_args(namespace)) as runner:
        objects_api.create_namespaced_custom_object(
            group, version, namespace, plural, cr_body
        )
        _wait_ready(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
        )

        new_password = f"rotated-{secret_name}"
        core_api.patch_namespaced_secret(
            secret_name,
            namespace,
            {"data": {"password": base64.b64encode(new_password.encode()).decode()}},
            _content_type="application/merge-patch+json",
        )

        firebird.wait_for_sysdba_ready(
            core_api,
            namespace=namespace,
            pod_name=f"{name}-0",
            container="firebird",
            sysdba_password=new_password,
            logger=logging.getLogger(__name__),
            timeout=60,
        )

        objects_api.delete_namespaced_custom_object(
            group, version, namespace, plural, name
        )
        time.sleep(1)

    assert runner.exit_code == 0
    assert runner.exception is None
    assert "Handler 'sysdba_secret_update_fn' succeeded." in runner.output


def test_update_sysdba_secret_password_secretref(kubeconfig: Path) -> None:
    """sysdba_secret_update_fn is expected to also cover a user-provided
    authentication.sysdba.secretRef, once kubebird has labeled it."""
    config.load_kube_config(config_file=str(kubeconfig))

    crd_body = yaml.safe_load(CRD_PATH.read_text())
    cr_body = yaml.safe_load(CR_PATH.read_text())
    cr_body["metadata"]["name"] = "test-update-sysdba-ref"

    group, version = cr_body["apiVersion"].split("/")
    plural = crd_body["spec"]["names"]["plural"]
    namespace = "default"
    name = cr_body["metadata"]["name"]
    secret_name = f"{name}-custom-sysdba"
    initial_password = f"initial-{secret_name}"
    cr_body["spec"]["authentication"]["sysdba"]["secretRef"] = secret_name

    extensions_api = client.ApiextensionsV1Api()
    _ensure_crd_established(extensions_api, crd_body)

    objects_api = client.CustomObjectsApi()
    core_api = client.CoreV1Api()

    core_api.create_namespaced_secret(
        namespace,
        {
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {"name": secret_name, "namespace": namespace},
            "stringData": {"username": "SYSDBA", "password": initial_password},
        },
    )

    with KopfRunner(_runner_args(namespace)) as runner:
        objects_api.create_namespaced_custom_object(
            group, version, namespace, plural, cr_body
        )
        _wait_ready(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
        )

        new_password = f"rotated-{secret_name}"
        core_api.patch_namespaced_secret(
            secret_name,
            namespace,
            {"data": {"password": base64.b64encode(new_password.encode()).decode()}},
            _content_type="application/merge-patch+json",
        )

        firebird.wait_for_sysdba_ready(
            core_api,
            namespace=namespace,
            pod_name=f"{name}-0",
            container="firebird",
            sysdba_password=new_password,
            logger=logging.getLogger(__name__),
            timeout=60,
        )

        objects_api.delete_namespaced_custom_object(
            group, version, namespace, plural, name
        )
        time.sleep(1)

    core_api.delete_namespaced_secret(secret_name, namespace)

    assert runner.exit_code == 0
    assert runner.exception is None
    assert "Handler 'sysdba_secret_update_fn' succeeded." in runner.output


def test_update_instance_add_database(kubeconfig: Path) -> None:
    """update_fn is expected to provision databases added to spec.databases after creation."""
    config.load_kube_config(config_file=str(kubeconfig))

    crd_body = yaml.safe_load(CRD_PATH.read_text())
    cr_body = yaml.safe_load(CR_PATH.read_text())
    cr_body["metadata"]["name"] = "test-update-database"
    primary_db = next(db for db in cr_body["spec"]["databases"] if not db.get("shadow"))
    cr_body["spec"]["databases"] = [primary_db]

    group, version = cr_body["apiVersion"].split("/")
    plural = crd_body["spec"]["names"]["plural"]
    namespace = "default"
    name = cr_body["metadata"]["name"]

    extensions_api = client.ApiextensionsV1Api()
    _ensure_crd_established(extensions_api, crd_body)

    objects_api = client.CustomObjectsApi()

    with KopfRunner(_runner_args(namespace)) as runner:
        objects_api.create_namespaced_custom_object(
            group, version, namespace, plural, cr_body
        )
        _wait_ready(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
        )

        new_db = {"name": "extra.fdb", "shadow": False}
        objects_api.patch_namespaced_custom_object(
            group,
            version,
            namespace,
            plural,
            name,
            {"spec": {"databases": [primary_db, new_db]}},
        )
        _wait_status_message(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
            message="Instance updated.",
        )

        _assert_database_file_exists(
            client.CoreV1Api(),
            namespace=namespace,
            pod_name=f"{name}-0",
            path=f"{DATA_MOUNT_PATH}/{new_db['name']}",
        )

        # The pod never restarted for this change, so this only passes if
        # update_fn actually exec'd the new databases.conf content into the
        # live container -- the ConfigMap's own subPath mount would not have
        # picked it up on its own.
        databases_conf = _read_databases_conf(
            client.CoreV1Api(), namespace=namespace, pod_name=f"{name}-0"
        )
        assert (
            f"{new_db['name']} = {DATA_MOUNT_PATH}/{new_db['name']}" in databases_conf
        )

        objects_api.delete_namespaced_custom_object(
            group, version, namespace, plural, name
        )
        time.sleep(1)

    assert runner.exit_code == 0
    assert runner.exception is None
    assert "Handler 'update_fn' succeeded." in runner.output
