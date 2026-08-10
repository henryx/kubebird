#!/usr/bin/env python3
"""Entry point to run the Kubebird operator embedded, on uvloop.

kopf's CLI (``kopf run``) picks up uvloop automatically when it is installed,
but that auto-detection is CLI-only -- ``kopf.run()`` does not manage the
event loop for you when embedded like this. Per kopf's own docs, the
supported way to run under uvloop here is to build the loop ourselves and
pass it in.
"""

import asyncio
import os

import kopf
import uvloop

# Import for their @kopf.on.* decorators' side effect: registering handlers
# in kopf's default registry. kopf.run() only sees handlers from modules that
# have actually been imported by the time it starts.
from kubebird import create, delete, update  # noqa: F401


def main() -> None:
    namespace = os.environ.get("NAMESPACE")
    namespaces = [namespace] if namespace else ()
    with asyncio.Runner(loop_factory=uvloop.new_event_loop) as runner:
        kopf.run(loop=runner.get_loop(), namespaces=namespaces)


if __name__ == "__main__":
    main()
