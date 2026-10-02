# qoj_post_processing dashboard: same deployment shape as skQCI (Flask + gunicorn behind Caddy, run as a non-root user matching the host
# owner of the bind-mounted keys/recordings/config dirs), plus the Go toolchain needed to build the qoj_post_processing CLI this app shells out to

FROM python:3.12-slim

ARG GO_VERSION=1.24.0
ARG TARGETARCH=amd64
ARG UID=1000
ARG GID=1000

RUN apt-get update \
    && apt-get install -y --no-install-recommends curl ca-certificates \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" | tar -C /usr/local -xz \
    && apt-get purge -y curl \
    && rm -rf /var/lib/apt/lists/*

ENV PATH="/usr/local/go/bin:${PATH}"
ENV GOCACHE=/tmp/gocache

WORKDIR /app

# UI's own deps (Flask/gunicorn) plus coincidence_peak.py's (numpy/numba/scipy),
# since /api/align/live imports it directly rather than shelling out
COPY ui/requirements.txt /tmp/ui-requirements.txt
RUN pip install --no-cache-dir -r /tmp/ui-requirements.txt

# Whole repo: the Go module (to build the CLI) and acquisition/ (imported by app.py)
COPY . /app

RUN go build -o /usr/local/bin/qoj_post_processing ./cmd/qoj_post_processing \
 && go build -o /usr/local/bin/importtt ./cmd/importtt

RUN groupadd -g ${GID} app 2>/dev/null || true \
 && useradd -u ${UID} -g ${GID} -m -s /usr/sbin/nologin app 2>/dev/null || true \
 && mkdir -p /app/keys /app/recordings /app/config \
 && chown -R ${UID}:${GID} /app

ENV QOJ_BIN=/usr/local/bin/qoj_post_processing
ENV IMPORTTT_BIN=/usr/local/bin/importtt
ENV KEYSTORE_DIR=/app/keys
ENV RECORDINGS_DIR=/app/recordings
ENV CONFIG_DIR=/app/config

USER ${UID}:${GID}

EXPOSE 9823

WORKDIR /app/ui
# --timeout is generous (not gunicorn's 30s default): huge recording upload over a slow link can legitimately take a long time,
# request must stay alive for the whole transfer, not just the size check at the end of it
# --workers 1: app.py keeps all of its state (acquisition status/log, analysis cache, simulated-key tracking, CHSH cache)
# as plain in-process globals, not in a shared store
CMD ["gunicorn", "app:app", "--bind", "0.0.0.0:9823", "--workers", "1", "--threads", "4", "--worker-class", "gthread", "--timeout", "3600"]
