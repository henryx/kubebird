from typing import Any

import kopf


@kopf.on.update(kind="Instance", version="v1", group="kubebird.github.io")
def update_fn(
    spec: kopf.Spec,
    name: str,
    namespace: str,
    logger: kopf.Logger,
    body: kopf.Body,
    patch: kopf.Patch,
    **_: Any,
) -> None:
    raise NotImplementedError("Not implemented")
