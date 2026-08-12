"""Entry point to run the Kubebird operator embedded, on uvloop.

kopf's CLI (``kopf run``) picks up uvloop automatically when it is installed,
but that auto-detection is CLI-only -- ``kopf.run()`` does not manage the
event loop for you when embedded like this. Per kopf's own docs, the
supported way to run under uvloop here is to build the loop ourselves and
pass it in.
"""

import asyncio
import logging
import os

import kopf
import uvloop

# Import for their @kopf.on.* decorators' side effect: registering handlers
# in kopf's default registry. kopf.run() only sees handlers from modules that
# have actually been imported by the time it starts.
from kubebird import create, delete, update  # noqa: F401

DEFAULT_LOG_LEVEL = "INFO"
_VALID_LOG_LEVELS = {"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"}


def _resolve_log_level() -> str:
    level = os.environ.get("LOG_LEVEL", DEFAULT_LOG_LEVEL).upper()
    return level if level in _VALID_LOG_LEVELS else DEFAULT_LOG_LEVEL


def main() -> None:
    # kopf.configure() is kopf's documented hook for controlling logging when
    # embedding kopf.run() directly (as opposed to the `kopf run` CLI's own
    # --verbose/--debug/--quiet flags): it installs kopf's formatter/handler on
    # the root logger, and -- only when debug=True -- lets kopf's own internal
    # (e.g. asyncio) logging propagate instead of being muted. Its debug/
    # verbose/quiet knobs only distinguish DEBUG/INFO/WARNING though, so the
    # level is set explicitly right after to whatever LOG_LEVEL actually asks
    # for (also covering ERROR/CRITICAL, which configure() has no knob for).
    log_level = _resolve_log_level()
    kopf.configure(
        debug=(log_level == "DEBUG"),
        verbose=(log_level == "DEBUG"),
        quiet=(log_level == "WARNING"),
    )
    logging.getLogger().setLevel(log_level)

    namespace = os.environ.get("NAMESPACE")
    namespaces = [namespace] if namespace else ()
    with asyncio.Runner(loop_factory=uvloop.new_event_loop) as runner:
        kopf.run(loop=runner.get_loop(), namespaces=namespaces)
