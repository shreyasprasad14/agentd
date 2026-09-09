# agentd/sandbox:python — the image every `python` tool call runs in.
#
# The hard boundaries (no network, memory/cpu/pids caps, read-only rootfs,
# dropped capabilities, no-new-privileges, uid 65534) are applied by the
# executor at container creation, not here; see internal/sandbox/docker.go.
# This image only makes the payload runtime small and predictable.
FROM python:3.12-slim-bookworm

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    HOME=/tmp

# Standard library only: no pip, no setuptools. There is no network to install
# anything with anyway; removing them makes that literal and trims the image.
RUN pip uninstall -y pip setuptools wheel >/dev/null 2>&1 || true \
    && rm -rf /root/.cache /usr/local/lib/python3.12/ensurepip

# Scripts and input files are copied here before the container starts. The
# rootfs is mounted read-only at runtime, so nothing can modify them after.
RUN mkdir -p /work/in && chmod 0555 /work /work/in

USER 65534:65534
WORKDIR /tmp
CMD ["python3"]
