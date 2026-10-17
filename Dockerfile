# syntax=docker/dockerfile:1

# Three images from one file: kms (key management system, PKI tool), app (dashboard, pipeline), tools (every command)
# Go cross-compiles on the build machine for each target, so amd64 and arm64 nodes build from the same file

ARG GO_VERSION=1.24
ARG PYTHON_VERSION=3.12

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY qkd/ qkd/
COPY kms/ kms/
COPY relay/ relay/
COPY pyec/ pyec/
COPY cmd/ cmd/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/ ./cmd/...

# Static binaries on a distroless base: no shell, no package manager, non-root by default
FROM gcr.io/distroless/static-debian12:nonroot AS kms
COPY --from=build /out/kms /out/qkdpki /usr/local/bin/
EXPOSE 8443
ENTRYPOINT ["/usr/local/bin/kms"]

FROM python:${PYTHON_VERSION}-slim AS app
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
COPY ui/requirements.txt /tmp/requirements.txt
COPY ec/requirements.txt /tmp/ec-requirements.txt
RUN pip install -r /tmp/requirements.txt -r /tmp/ec-requirements.txt && rm /tmp/*requirements.txt \
 && useradd -u 1000 -m -s /usr/sbin/nologin app
COPY --from=build /out/qkd_post_processing /out/importtt /usr/local/bin/
COPY acquisition/*.py /app/acquisition/
# evaluation/evaluate.py: the dashboard's Evaluation page and the tools image run it
COPY evaluation/*.py /app/evaluation/
# 5G NR LDPC's matrix work, run by the Go pipeline as a pool of service processes (pyec)
COPY ec/ldpc5g.py /app/ec/
COPY ui/app.py /app/ui/
COPY ui/templates/ /app/ui/templates/
COPY ui/static/ /app/ui/static/
# Every writable path is a mounted volume or /tmp, so the container runs with a read-only root
ENV QKD_BIN=/usr/local/bin/qkd_post_processing IMPORTTT_BIN=/usr/local/bin/importtt \
    RECORDINGS_DIR=/data/recordings CONFIG_DIR=/data/config IMPORT_DIR=/data/import \
    CONTINUOUS_DIR=/data/continuous EVAL_DIR=/data/evaluations \
    KEYSTORE_DIR=/data/sim-keys NUMBA_CACHE_DIR=/data/cache/numba MPLCONFIGDIR=/tmp \
    QKD_EC_PYTHON=python QKD_EC_SCRIPT=/app/ec/ldpc5g.py HOME=/tmp
USER 1000:1000
WORKDIR /app/ui
EXPOSE 9823
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s \
  CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:9823/healthz', timeout=4)"]
# One worker: jobs, caches and the acquisition process live in this process; threads serve requests
CMD ["gunicorn", "app:app", "--bind", "0.0.0.0:9823", "--workers", "1", "--threads", "8", \
     "--worker-class", "gthread", "--timeout", "3600", "--worker-tmp-dir", "/tmp"]

# Command-line image for offline analysis: every binary, for coincidence_peak.py, evaluate.py and the CLIs
FROM app AS tools
USER root
COPY --from=build /out/ /usr/local/bin/
USER 1000:1000
WORKDIR /work
CMD ["bash"]
