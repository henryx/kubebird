"""Firebird database/user provisioning via `isql`, executed inside the instance's pod."""

import base64
import shlex
import time

import kopf
from kubernetes import client
from kubernetes.stream import stream

from . import k8s

POD_READY_TIMEOUT = 180.0
POD_READY_POLL_INTERVAL = 2.0
SYSDBA_READY_TIMEOUT = 120.0
SYSDBA_READY_POLL_INTERVAL = 3.0
SYSDBA_PROBE_PATH = "/tmp/kubebird-readiness-probe.fdb"


def _redact(text: str, *secrets: str) -> str:
    """Strip literal secret values (e.g. a SYSDBA password) out of a string
    before it's logged -- even at DEBUG level, exec commands/SQL text below
    embed passwords inline, and kubebird has no control over where the
    resulting log lines end up (stdout, an aggregator, etc.)."""
    for secret in secrets:
        if secret:
            text = text.replace(secret, "***")
    return text


def wait_for_pod_ready(
    core_api: client.CoreV1Api,
    *,
    namespace: str,
    pod_name: str,
    logger: kopf.Logger,
    timeout: float = POD_READY_TIMEOUT,
) -> None:
    logger.info(f"Waiting for pod {pod_name!r} to become Ready.")
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        # read_namespaced_pod, not read_namespaced_pod_status: the latter hits
        # the pods/status *subresource* endpoint, which RBAC treats as a
        # distinct resource from plain "pods" and so needs its own grant --
        # the main resource already returns the identical .status.conditions
        # for a GET, so there's no need to widen the ServiceAccount's Role for
        # this.
        pod = core_api.read_namespaced_pod(pod_name, namespace)
        conditions = pod.status.conditions or []
        if any(c.type == "Ready" and c.status == "True" for c in conditions):
            logger.debug(f"Pod {pod_name!r} is Ready.")
            return
        time.sleep(POD_READY_POLL_INTERVAL)
    raise kopf.TemporaryError(
        f"Pod {pod_name!r} did not become Ready in time.", delay=10
    )


def wait_for_sysdba_ready(
    core_api: client.CoreV1Api,
    *,
    namespace: str,
    pod_name: str,
    container: str,
    sysdba_password: str,
    logger: kopf.Logger,
    timeout: float = SYSDBA_READY_TIMEOUT,
) -> None:
    """Block until the server actually accepts `sysdba_password`.

    The image's entrypoint applies FIREBIRD_ROOT_PASSWORD after the process
    starts, so the container can be Ready (no readiness probe distinguishes
    this) before that change has taken effect. A throwaway probe database is
    created and dropped as a proxy for "authentication is live".
    """
    logger.info(
        f"Waiting for SYSDBA authentication to become live on pod {pod_name!r}."
    )
    sql = (
        f"CREATE DATABASE 'localhost:{SYSDBA_PROBE_PATH}' "
        f"USER 'SYSDBA' PASSWORD '{sysdba_password}';\nDROP DATABASE;"
    )
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        output = run_isql(
            core_api,
            namespace=namespace,
            pod_name=pod_name,
            container=container,
            sysdba_password=sysdba_password,
            sql=sql,
            logger=logger,
            check=False,
        )
        if "Statement failed" not in output:
            logger.debug("SYSDBA authentication is live.")
            return
        time.sleep(SYSDBA_READY_POLL_INTERVAL)
    raise kopf.TemporaryError(
        "SYSDBA authentication did not become ready in time.", delay=10
    )


def run_isql(
    core_api: client.CoreV1Api,
    *,
    namespace: str,
    pod_name: str,
    container: str,
    sysdba_password: str,
    sql: str,
    logger: kopf.Logger,
    database: str | None = None,
    check: bool = True,
) -> str:
    # A bare local path makes isql open the database through Firebird's
    # embedded/local provider, which races the already-running SuperServer
    # for an exclusive lock on the security database (breaking CREATE USER).
    # Connecting via "localhost:" instead routes through that same server.
    target = shlex.quote(f"localhost:{database}") if database else ""
    shell_command = (
        f"echo {shlex.quote(sql)} | isql -quiet "
        f"-user SYSDBA -password {shlex.quote(sysdba_password)} {target}"
    )
    command = ["/bin/sh", "-c", shell_command]
    output = stream(
        core_api.connect_get_namespaced_pod_exec,
        pod_name,
        namespace,
        container=container,
        command=command,
        stderr=True,
        stdin=False,
        stdout=True,
        tty=False,
    )
    logger.debug(
        f"isql command={_redact(shell_command, sysdba_password)!r} output={output!r}"
    )
    # The exec exit-code channel varies across negotiated websocket
    # subprotocol versions and is unreliable here; isql itself always
    # prints this marker on a failed statement, so check for it instead.
    if check and "Statement failed" in output:
        raise kopf.PermanentError(f"isql failed (sql={sql!r}): {output}")
    return output


def change_sysdba_password(
    core_api: client.CoreV1Api,
    *,
    namespace: str,
    pod_name: str,
    container: str,
    old_password: str,
    new_password: str,
    logger: kopf.Logger,
) -> str:
    """Change the live SYSDBA password via `gsec`, given the currently-live password."""
    shell_command = (
        f"gsec -user SYSDBA -password {shlex.quote(old_password)} "
        f"-modify SYSDBA -pw {shlex.quote(new_password)}"
    )
    command = ["/bin/sh", "-c", shell_command]
    output = stream(
        core_api.connect_get_namespaced_pod_exec,
        pod_name,
        namespace,
        container=container,
        command=command,
        stderr=True,
        stdin=False,
        stdout=True,
        tty=False,
    )
    logger.debug(
        f"gsec command={_redact(shell_command, old_password, new_password)!r} "
        f"output={output!r}"
    )
    # gsec prints nothing on success; any output is an error (e.g. the old
    # password no longer matching what's actually live).
    if output.strip():
        raise kopf.PermanentError(
            f"gsec failed to change the SYSDBA password: {output}"
        )
    return output


def write_databases_conf(
    core_api: client.CoreV1Api,
    *,
    namespace: str,
    pod_name: str,
    container: str,
    content: str,
    logger: kopf.Logger,
) -> None:
    """Overwrite databases.conf inside the already-running container.

    This path is backed by a writable emptyDir, seeded from the databases.conf
    ConfigMap by an initContainer on pod (re)start (see k8s.build_statefulset;
    a ConfigMap volume itself is always mounted read-only, subPath or not, so
    it can't be the live target of this exec). This is what makes a new/changed
    alias usable immediately, without waiting for (or forcing) a pod restart.
    Content is base64-encoded over the wire since database names (and
    therefore alias names) come from user-controlled CR fields, not to be
    trusted as literal shell/heredoc text.
    """
    encoded = base64.b64encode(content.encode()).decode()
    shell_command = f"echo {shlex.quote(encoded)} | base64 -d > {shlex.quote(k8s.DATABASES_CONF_PATH)}"
    command = ["/bin/sh", "-c", shell_command]
    output = stream(
        core_api.connect_get_namespaced_pod_exec,
        pod_name,
        namespace,
        container=container,
        command=command,
        stderr=True,
        stdin=False,
        stdout=True,
        tty=False,
    )
    logger.debug(f"write_databases_conf command={command!r} output={output!r}")
    if output.strip():
        raise kopf.PermanentError(f"Failed to write databases.conf: {output}")


def create_database(
    core_api: client.CoreV1Api,
    *,
    namespace: str,
    pod_name: str,
    container: str,
    sysdba_password: str,
    path: str,
    page_size: int,
    charset: str,
    collation: str,
    logger: kopf.Logger,
) -> str:
    sql = (
        f"CREATE DATABASE 'localhost:{path}' USER 'SYSDBA' PASSWORD '{sysdba_password}' "
        f"PAGE_SIZE {page_size} DEFAULT CHARACTER SET {charset} COLLATION {collation};"
    )
    return run_isql(
        core_api,
        namespace=namespace,
        pod_name=pod_name,
        container=container,
        sysdba_password=sysdba_password,
        sql=sql,
        logger=logger,
    )


def create_shadow(
    core_api: client.CoreV1Api,
    *,
    namespace: str,
    pod_name: str,
    container: str,
    sysdba_password: str,
    database_path: str,
    shadow_path: str,
    logger: kopf.Logger,
) -> str:
    sql = f"CREATE SHADOW 1 '{shadow_path}';"
    return run_isql(
        core_api,
        namespace=namespace,
        pod_name=pod_name,
        container=container,
        sysdba_password=sysdba_password,
        sql=sql,
        database=database_path,
        logger=logger,
    )
