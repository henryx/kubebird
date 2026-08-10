from typing import Any

import kopf


@kopf.on.delete(kind="Instance", version="v1", group="kubebird.github.io")
def delete_fn(
    spec: kopf.Spec,
    name: str,
    namespace: str,
    logger: kopf.Logger,
    body: kopf.Body,
    patch: kopf.Patch,
    **_: Any,
) -> None:
    raise NotImplementedError("Not implemented")
