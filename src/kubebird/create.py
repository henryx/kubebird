from typing import Any

import kopf


@kopf.on.create(kind="Instance", version="v1", group="kubebird.github.io")
def create_fn(
    spec: kopf.Spec, name: str, namespace: str | None, logger: kopf.Logger, **_: Any
) -> None:
    pass
