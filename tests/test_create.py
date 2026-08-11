import shlex
import time
from pathlib import Path

import yaml
from kopf.testing import KopfRunner
from kubernetes import client, config
from kubernetes.client.exceptions import ApiException
from kubernetes.stream import stream

from kubebird.create import CONTAINER_NAME
from kubebird.k8s import DATA_MOUNT_PATH, SHADOW_MOUNT_PATH

DEPLOY_DIR = Path(__file__).resolve().parent.parent / "deploy"
CRD_PATH = DEPLOY_DIR / "crd.yaml"
CR_PATH = DEPLOY_DIR / "cr.yaml"


def _ensure_crd_established(
    api: client.ApiextensionsV1Api, crd_body: dict, timeout: float = 30.0
) -> None:
    """Create the (cluster-scoped) CRD, tolerating it already existing from another test."""
    name = crd_body["metadata"]["name"]
    try:
        api.create_custom_resource_definition(crd_body)
    except ApiException as e:
        if e.status != 409:
            raise

    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        crd = api.read_custom_resource_definition(name)
        conditions = crd.status.conditions or []
        if any(c.type == "Established" and c.status == "True" for c in conditions):
            return
        time.sleep(0.5)
    raise TimeoutError(f"CustomResourceDefinition {name!r} did not become Established")


def _wait_ready(
    api: client.CustomObjectsApi,
    *,
    group: str,
    version: str,
    namespace: str,
    plural: str,
    name: str,
    timeout: float = 420.0,
) -> dict:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        obj = api.get_namespaced_custom_object(group, version, namespace, plural, name)
        phase = (obj.get("status") or {}).get("phase")
        if phase == "Ready":
            return obj
        time.sleep(2)
    raise TimeoutError(f"Instance {name!r} did not reach phase Ready in time")


def _assert_database_file_exists(
    core_api: client.CoreV1Api, *, namespace: str, pod_name: str, path: str
) -> None:
    output = stream(
        core_api.connect_get_namespaced_pod_exec,
        pod_name,
        namespace,
        container=CONTAINER_NAME,
        command=["/bin/sh", "-c", f"test -f {shlex.quote(path)} && echo EXISTS"],
        stderr=True,
        stdin=False,
        stdout=True,
        tty=False,
    )
    assert "EXISTS" in output, f"database file {path!r} was not created: {output}"


def test_create_instance(kubeconfig: Path) -> None:
    config.load_kube_config(config_file=str(kubeconfig))

    crd_body = yaml.safe_load(CRD_PATH.read_text())
    cr_body = yaml.safe_load(CR_PATH.read_text())

    group, version = cr_body["apiVersion"].split("/")
    plural = crd_body["spec"]["names"]["plural"]
    namespace = "default"
    name = cr_body["metadata"]["name"]

    extensions_api = client.ApiextensionsV1Api()
    _ensure_crd_established(extensions_api, crd_body)

    objects_api = client.CustomObjectsApi()

    with KopfRunner(
        ["run", "-n", namespace, "--verbose", "-m", "kubebird.create"]
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

        primary_db = next(
            db for db in cr_body["spec"]["databases"] if not db.get("shadow")
        )
        _assert_database_file_exists(
            client.CoreV1Api(),
            namespace=namespace,
            pod_name=f"{name}-0",
            path=f"{DATA_MOUNT_PATH}/{primary_db['name']}",
        )
        objects_api.delete_namespaced_custom_object(
            group, version, namespace, plural, name
        )
        time.sleep(1)

    assert runner.exit_code == 0
    assert runner.exception is None
    assert "Handler 'create_fn' succeeded." in runner.output


def test_create_instance_shadow_database(kubeconfig: Path) -> None:
    config.load_kube_config(config_file=str(kubeconfig))

    crd_body = yaml.safe_load(CRD_PATH.read_text())
    cr_body = yaml.safe_load(CR_PATH.read_text())
    cr_body["metadata"]["name"] = "test-shadow"

    group, version = cr_body["apiVersion"].split("/")
    plural = crd_body["spec"]["names"]["plural"]
    namespace = "default"
    name = cr_body["metadata"]["name"]

    extensions_api = client.ApiextensionsV1Api()
    _ensure_crd_established(extensions_api, crd_body)

    objects_api = client.CustomObjectsApi()

    with KopfRunner(
        ["run", "-n", namespace, "--verbose", "-m", "kubebird.create"]
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

        shadow_db = next(db for db in cr_body["spec"]["databases"] if db.get("shadow"))
        _assert_database_file_exists(
            client.CoreV1Api(),
            namespace=namespace,
            pod_name=f"{name}-0",
            path=f"{SHADOW_MOUNT_PATH}/{shadow_db['name']}.shadow",
        )
        objects_api.delete_namespaced_custom_object(
            group, version, namespace, plural, name
        )
        time.sleep(1)

    assert runner.exit_code == 0
    assert runner.exception is None
    assert "Handler 'create_fn' succeeded." in runner.output
