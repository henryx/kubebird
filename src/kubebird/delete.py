from typing import Any

import kopf


@kopf.on.delete(kind="Instance", version="v1", group="kubebird.github.io")
def delete_fn(
    name: str,
    namespace: str,
    logger: kopf.Logger,
    patch: kopf.Patch,
    **_: Any,
) -> None:
    """Nothing to actively clean up here: every object create_fn/update_fn
    made was adopted via kopf.adopt(), so Kubernetes garbage-collects all of
    them (Secret, PVC(s), Service, StatefulSet) through their owner
    references as soon as this handler returns and kopf drops the finalizer
    it adds for having any on.delete handler registered at all.
    """
    logger.info(f"Deleting instance {name!r} in namespace {namespace!r}.")
    patch.status["phase"] = "Deleting"
