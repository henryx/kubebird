from collections.abc import Iterator

import pytest
from testcontainers.community.k3s import K3SContainer


@pytest.fixture(scope="session")
def k3s() -> Iterator[K3SContainer]:
    with K3SContainer() as container:
        yield container
