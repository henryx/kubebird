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
    _wait_ready,
)

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


def _wait_service_type(
    api: client.CoreV1Api,
    *,
    namespace: str,
    name: str,
    expected_type: str,
    timeout: float = 60.0,
) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        service = api.read_namespaced_service(name, namespace)
        if service.spec.type == expected_type:
            return
        time.sleep(2)
    raise TimeoutError(
        f"Service {name!r} did not become type {expected_type!r} in time"
    )


def _wait_statefulset_image(
    api: client.AppsV1Api,
    *,
    namespace: str,
    name: str,
    expected_image: str,
    timeout: float = 60.0,
) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        statefulset = api.read_namespaced_stateful_set(name, namespace)
        image = statefulset.spec.template.spec.containers[0].image
        if image == expected_image:
            return
        time.sleep(2)
    raise TimeoutError(
        f"StatefulSet {name!r} was not updated to image {expected_image!r} in time"
    )


def test_update_instance_service_type(kubeconfig: Path) -> None:
    """update_fn is expected to reconcile the Service when spec.service.type changes."""
    config.load_kube_config(config_file=str(kubeconfig))

    crd_body = yaml.safe_load(CRD_PATH.read_text())
    cr_body = yaml.safe_load(CR_PATH.read_text())
    cr_body["metadata"]["name"] = "test-update-service"

    group, version = cr_body["apiVersion"].split("/")
    plural = crd_body["spec"]["names"]["plural"]
    namespace = cr_body["metadata"]["namespace"]
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
        _wait_service_type(
            core_api, namespace=namespace, name=name, expected_type="NodePort"
        )
        instance = _wait_ready(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
        )
        assert instance["status"]["phase"] == "Ready"

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
    namespace = cr_body["metadata"]["namespace"]
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
        _wait_statefulset_image(
            apps_api,
            namespace=namespace,
            name=name,
            expected_image=f"{image}:{new_version}",
        )
        # The StatefulSet's rolling update needs to pull the new image and
        # restart the pod, so re-provisioning takes as long as a fresh create.
        instance = _wait_ready(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
        )
        assert instance["status"]["phase"] == "Ready"

        objects_api.delete_namespaced_custom_object(
            group, version, namespace, plural, name
        )
        time.sleep(1)

    assert runner.exit_code == 0
    assert runner.exception is None
    assert "Handler 'update_fn' succeeded." in runner.output


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
    namespace = cr_body["metadata"]["namespace"]
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
        instance = _wait_ready(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
        )
        assert instance["status"]["phase"] == "Ready"

        _assert_database_file_exists(
            client.CoreV1Api(),
            namespace=namespace,
            pod_name=f"{name}-0",
            path=f"{DATA_MOUNT_PATH}/{new_db['name']}",
        )

        objects_api.delete_namespaced_custom_object(
            group, version, namespace, plural, name
        )
        time.sleep(1)

    assert runner.exit_code == 0
    assert runner.exception is None
    assert "Handler 'update_fn' succeeded." in runner.output
