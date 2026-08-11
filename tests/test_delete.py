import time
from pathlib import Path

import yaml
from kopf.testing import KopfRunner
from kubernetes import client, config
from kubernetes.client.exceptions import ApiException
from test_create import CR_PATH, CRD_PATH, _ensure_crd_established, _wait_ready


def _wait_gone(
    api: client.CustomObjectsApi,
    *,
    group: str,
    version: str,
    namespace: str,
    plural: str,
    name: str,
    timeout: float = 60.0,
) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            api.get_namespaced_custom_object(group, version, namespace, plural, name)
        except ApiException as e:
            if e.status == 404:
                return
            raise
        time.sleep(2)
    raise TimeoutError(f"Instance {name!r} was not removed in time")


def _wait_statefulset_gone(
    api: client.AppsV1Api, *, namespace: str, name: str, timeout: float = 60.0
) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            api.read_namespaced_stateful_set(name, namespace)
        except ApiException as e:
            if e.status == 404:
                return
            raise
        time.sleep(2)
    raise TimeoutError(f"StatefulSet {name!r} was not garbage-collected in time")


def test_delete_instance(kubeconfig: Path) -> None:
    """delete_fn is expected to run as a real @kopf.on.delete finalizer handler.

    Registering any on.delete handler makes kopf add a finalizer to the
    Instance, which blocks Kubernetes from actually removing the object until
    the handler returns without raising. Once it does, kopf drops the
    finalizer, the Instance disappears, and Kubernetes garbage-collects every
    object kubebird adopted onto it (Secret, PVC(s), Service, StatefulSet) via
    their owner references -- exactly as already happens today without a
    delete_fn at all, just no longer racing the Instance object's own removal.
    """
    config.load_kube_config(config_file=str(kubeconfig))

    crd_body = yaml.safe_load(CRD_PATH.read_text())
    cr_body = yaml.safe_load(CR_PATH.read_text())
    cr_body["metadata"]["name"] = "test-delete"

    group, version = cr_body["apiVersion"].split("/")
    plural = crd_body["spec"]["names"]["plural"]
    namespace = "default"
    name = cr_body["metadata"]["name"]

    extensions_api = client.ApiextensionsV1Api()
    _ensure_crd_established(extensions_api, crd_body)

    objects_api = client.CustomObjectsApi()
    apps_api = client.AppsV1Api()

    with KopfRunner(
        [
            "run",
            "-n",
            namespace,
            "--verbose",
            "-m",
            "kubebird.create",
            "-m",
            "kubebird.delete",
        ]
    ) as runner:
        objects_api.create_namespaced_custom_object(
            group, version, namespace, plural, cr_body
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

        _wait_gone(
            objects_api,
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
        )
        _wait_statefulset_gone(apps_api, namespace=namespace, name=name)

    assert runner.exit_code == 0
    assert runner.exception is None
    assert "Handler 'delete_fn' succeeded." in runner.output
