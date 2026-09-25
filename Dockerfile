# Packages the whole toolchain (Go pipeline + Python acquisition scripts)
# into one image, no server, no UI -- just run any of the repo's commands
# through `docker run`.
FROM python:3.12-slim

ARG GO_VERSION=1.24.0
ARG TARGETARCH=amd64

RUN apt-get update \
    && apt-get install -y --no-install-recommends curl ca-certificates \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" | tar -C /usr/local -xz \
    && apt-get purge -y curl \
    && rm -rf /var/lib/apt/lists/*

ENV PATH="/usr/local/go/bin:${PATH}"
ENV GOCACHE=/tmp/gocache

WORKDIR /app

COPY acquisition/requirements.txt /tmp/acquisition-requirements.txt
RUN pip install --no-cache-dir -r /tmp/acquisition-requirements.txt

COPY . /app

# Prebuilt binaries on PATH, so a container start is instant and neither
# the Go toolchain nor the source tree is needed at run time to use them
RUN go build -o /usr/local/bin/qoj_post_processing ./cmd/qoj_post_processing \
    && go build -o /usr/local/bin/importtt ./cmd/importtt \
    && go build -o /usr/local/bin/tuneblock ./cmd/tuneblock

CMD ["bash"]
