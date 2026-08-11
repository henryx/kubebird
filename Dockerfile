FROM registry.access.redhat.com/ubi10/python-314-minimal:1786427734 AS build

WORKDIR /app

COPY pyproject.toml /app
COPY README.md /app
COPY LICENSE /app
COPY src/ /app/src

RUN python3 -m venv /app/venv \
    && /app/venv/bin/pip install uv \
    && /app/venv/bin/uv build --wheel


FROM registry.access.redhat.com/ubi10/python-314-minimal:1786427734 AS user

USER root

RUN microdnf install -y shadow-utils \
    && microdnf clean all \
    && useradd -u 8877 appuser


FROM registry.access.redhat.com/ubi10/python-314-minimal:1786427734

ARG VERSION

LABEL description="Kubebird - A Kubernetes operator for Firebird"
LABEL version=$VERSION
LABEL authors="Enrico Bianchi <enrico.bianchi@gmail.com>"

USER root

WORKDIR /app

COPY --from=user /etc/passwd /etc/group /etc/
COPY --from=build /app/dist/*.whl /app/

RUN python3 -m venv /app/venv \
    && /app/venv/bin/pip install --no-cache-dir /app/*.whl \
    && rm -f /app/*.whl \
    && chown -R appuser:appuser /app

USER appuser

CMD ["/app/venv/bin/kubebird-operator"]