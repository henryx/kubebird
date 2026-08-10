import yaml
from testcontainers.community.k3s import K3SContainer


def test_k3s_config_yaml(k3s: K3SContainer) -> None:
    config = yaml.safe_load(k3s.config_yaml())

    assert config["apiVersion"] == "v1"
    assert config["clusters"][0]["cluster"]["server"] == (
        f"https://{k3s.get_container_host_ip()}:{k3s.get_exposed_port(k3s.KUBE_SECURE_PORT)}"
    )
