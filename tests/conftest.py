from collections.abc import Iterator
from pathlib import Path

import kubernetes.config.kube_config
import pytest
from testcontainers.community.k3s import K3SContainer


@pytest.fixture(scope="session")
def k3s() -> Iterator[K3SContainer]:
    container = K3SContainer()
    # On cgroup v2 hosts with the systemd cgroup driver, Docker gives the
    # container its own (private) cgroup namespace, which k3s's embedded
    # kubelet cannot reconcile with the host cgroup paths bind-mounted in --
    # every pod (even built-in ones like coredns) then stays Pending forever
    # with "FailedCreatePodSandBox: ... cgroup.procs: no such file or
    # directory". Sharing the host's cgroup namespace fixes it. K3SContainer
    # has no public API for extra `docker run` kwargs, hence the private attr.
    container._kwargs["cgroupns"] = "host"
    with container:
        yield container


@pytest.fixture
def kubeconfig(
    k3s: K3SContainer, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> Iterator[Path]:
    """Point kubectl, the kubernetes client, and kopf's client-based login at k3s.

    kubernetes.config.kube_config caches KUBECONFIG into a module-level constant
    the first time it is imported -- which happens as soon as `kopf` itself is
    imported, well before this fixture runs. Setting the env var alone is too
    late, so the constant is patched directly as well.
    """
    path = tmp_path / "kubeconfig.yaml"
    path.write_text(k3s.config_yaml())
    monkeypatch.setenv("KUBECONFIG", str(path))
    monkeypatch.setattr(
        kubernetes.config.kube_config, "KUBE_CONFIG_DEFAULT_LOCATION", str(path)
    )
    yield path
    path.unlink()
