# syntax=docker/dockerfile:1

# Three images from one file: kms (key management system, network controller, PKI tool), app (every node service and
# the pipeline), tools (every command) --> Go cross-compiles on the build machine, so amd64 and arm64 build alike

ARG GO_VERSION=1.26
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
COPY --from=build /out/kms /out/qkdpki /out/controller /usr/local/bin/
# kms on 8443; the controller runs from this image too, with --entrypoint controller, on 8080
EXPOSE 8443 8080
ENTRYPOINT ["/usr/local/bin/kms"]

FROM python:${PYTHON_VERSION}-slim AS app
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
COPY services/requirements.txt /tmp/requirements.txt
RUN pip install -r /tmp/requirements.txt && rm /tmp/requirements.txt \
 && useradd -u 1000 -m -s /usr/sbin/nologin app \
 && mkdir -p /data/tmp && chown -R 1000:1000 /data
COPY --from=build /out/qkd_post_processing /out/importtt /usr/local/bin/
COPY acquisition/*.py /app/acquisition/
COPY acquisition/coincidences/*.py /app/acquisition/coincidences/
# evaluation/evaluate.py: the dashboard's Evaluation page and the tools image run it
COPY evaluation/*.py /app/evaluation/
# one package, four services: SERVICE (or the first argument) picks agent, acquisition, processing, web or all
COPY services/ /app/services/
COPY ui/templates/ /app/ui/templates/
COPY ui/static/ /app/ui/static/
# Every writable path is a mounted volume or /tmp, so the container runs with a read-only root; /data belongs
# to the app user so that a fresh named volume mounted there does too
ENV QKD_BIN=/usr/local/bin/qkd_post_processing IMPORTTT_BIN=/usr/local/bin/importtt \
    RECORDINGS_DIR=/data/recordings CONFIG_DIR=/data/config IMPORT_DIR=/data/import \
    CONTINUOUS_DIR=/data/continuous EVAL_DIR=/data/evaluations CLOG_DIR=/data/coincidence_logs \
    KEYSTORE_DIR=/data/sim-keys NUMBA_CACHE_DIR=/data/cache/numba MPLCONFIGDIR=/tmp \
    HOME=/tmp
USER 1000:1000
WORKDIR /app
ENV SERVICE=all
# agent 9100, acquisition 9101, processing 9102, web and all 9823
EXPOSE 9100 9101 9102 9823
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD ["python", "-m", "services", "check"]
ENTRYPOINT ["python", "-m", "services"]

# Command-line image for offline analysis: every binary, for coincidence_peak.py, evaluate.py and the CLIs
FROM app AS tools
USER root
COPY --from=build /out/ /usr/local/bin/
USER 1000:1000
WORKDIR /work
HEALTHCHECK NONE
ENTRYPOINT []
CMD ["bash"]
