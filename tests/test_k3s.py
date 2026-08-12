from pathlib import Path

import yaml
from kubernetes import client, config
from kubernetes.client.exceptions import ApiException
from testcontainers.community.k3s import K3SContainer

DEPLOY_DIR = Path(__file__).resolve().parent.parent / "deploy"
CRD_PATH = DEPLOY_DIR / "crd.yaml"
OPERATOR_PATH = DEPLOY_DIR / "operator.yaml"


def test_k3s_config_yaml(k3s: K3SContainer) -> None:
    config = yaml.safe_load(k3s.config_yaml())

    assert config["apiVersion"] == "v1"
    assert config["clusters"][0]["cluster"]["server"] == (
        f"https://{k3s.get_container_host_ip()}:{k3s.get_exposed_port(k3s.KUBE_SECURE_PORT)}"
    )


def _create_object(kind: str, body: dict) -> None:
    # operator.yaml now hardcodes metadata.namespace on every namespaced object it
    # defines (kubebird-system), rather than relying on an apply-time -n flag.
    namespace = body.get("metadata", {}).get("namespace")
    if kind == "Namespace":
        try:
            client.CoreV1Api().create_namespace(body)
        except ApiException as e:
            if e.status != 409:
                raise
    elif kind == "ServiceAccount":
        client.CoreV1Api().create_namespaced_service_account(namespace, body)
    elif kind == "ClusterRole":
        client.RbacAuthorizationV1Api().create_cluster_role(body)
    elif kind == "ClusterRoleBinding":
        client.RbacAuthorizationV1Api().create_cluster_role_binding(body)
    elif kind == "Role":
        client.RbacAuthorizationV1Api().create_namespaced_role(namespace, body)
    elif kind == "RoleBinding":
        client.RbacAuthorizationV1Api().create_namespaced_role_binding(namespace, body)
    elif kind == "Deployment":
        client.AppsV1Api().create_namespaced_deployment(namespace, body)
    else:
        raise AssertionError(f"unexpected kind in operator.yaml: {kind!r}")


def _can(
    auth_api: client.AuthorizationV1Api,
    *,
    service_account: str,
    sa_namespace: str,
    resource_namespace: str,
    verb: str,
    resource: str,
    group: str = "",
    subresource: str | None = None,
) -> bool:
    review = client.V1SubjectAccessReview(
        spec=client.V1SubjectAccessReviewSpec(
            user=f"system:serviceaccount:{sa_namespace}:{service_account}",
            resource_attributes=client.V1ResourceAttributes(
                namespace=resource_namespace,
                verb=verb,
                group=group,
                resource=resource,
                subresource=subresource,
            ),
        )
    )
    result = auth_api.create_subject_access_review(review)
    return bool(result.status.allowed)


def test_operator_yaml_deploys_and_grants_expected_rbac(kubeconfig: Path) -> None:
    """deploy/operator.yaml is expected to apply cleanly (ServiceAccount, ClusterRole,
    ClusterRoleBinding, Role, RoleBinding, Deployment) and grant exactly the permissions
    create_fn/update_fn/k8s.py/firebird.py actually call, plus the cluster-scoped bits kopf's
    own framework needs (CRD/namespace observation) even though the operator itself is
    namespace-scoped."""
    config.load_kube_config(config_file=str(kubeconfig))

    namespace = "kubebird-system"
    service_account = "kubebird-operator"

    ext_api = client.ApiextensionsV1Api()
    crd_body = yaml.safe_load(CRD_PATH.read_text())
    try:
        ext_api.create_custom_resource_definition(crd_body)
    except ApiException as e:
        if e.status != 409:
            raise

    for doc in yaml.safe_load_all(OPERATOR_PATH.read_text()):
        _create_object(doc["kind"], doc)

    deployment = client.AppsV1Api().read_namespaced_deployment(
        service_account, namespace
    )
    assert deployment.spec.template.spec.service_account_name == service_account

    auth_api = client.AuthorizationV1Api()

    def can(
        verb: str, resource: str, *, group: str = "", subresource: str | None = None
    ) -> bool:
        return _can(
            auth_api,
            service_account=service_account,
            sa_namespace=namespace,
            resource_namespace=namespace,
            verb=verb,
            resource=resource,
            group=group,
            subresource=subresource,
        )

    def can_cluster(
        verb: str, resource: str, *, group: str = "", subresource: str | None = None
    ) -> bool:
        return _can(
            auth_api,
            service_account=service_account,
            sa_namespace=namespace,
            resource_namespace="",
            verb=verb,
            resource=resource,
            group=group,
            subresource=subresource,
        )

    # Application-level: exactly what create.py/update.py/k8s.py/firebird.py call.
    assert can("get", "instances", group="kubebird.github.io")
    assert can("list", "instances", group="kubebird.github.io")
    assert can("watch", "instances", group="kubebird.github.io")
    assert can("patch", "instances", group="kubebird.github.io")
    assert can("patch", "instances", group="kubebird.github.io", subresource="status")
    assert can("get", "secrets")
    assert can("create", "secrets")
    assert can("patch", "secrets")
    assert can("create", "configmaps")
    assert can("patch", "configmaps")
    assert can("create", "persistentvolumeclaims")
    assert can("create", "services")
    assert can("patch", "services")
    assert can("create", "statefulsets", group="apps")
    assert can("patch", "statefulsets", group="apps")
    assert can("get", "pods")
    assert can("create", "pods", subresource="exec")
    assert can("create", "events")

    # Framework-level: kopf's own cluster-scoped discovery needs, per its RBAC docs.
    assert can_cluster(
        "list", "customresourcedefinitions", group="apiextensions.k8s.io"
    )
    assert can_cluster(
        "watch", "customresourcedefinitions", group="apiextensions.k8s.io"
    )
    assert can_cluster("list", "namespaces")
    assert can_cluster("watch", "namespaces")
