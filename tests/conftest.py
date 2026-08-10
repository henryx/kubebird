from collections.abc import Iterator
from pathlib import Path

import kubernetes.config.kube_config
import pytest
from testcontainers.community.k3s import K3SContainer


@pytest.fixture(scope="session")
def k3s() -> Iterator[K3SContainer]:
    with K3SContainer() as container:
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
