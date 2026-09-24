ARG MIRASIM_VERSION=1.2.0
# ------------------------------------------------------------------
# Stage 1: build CPA server from this source tree (upstream v7.3.16).
# ------------------------------------------------------------------
FROM golang:1.26-bookworm AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=render
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=1 GOOS=linux go build -buildvcs=false \
  -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" \
  -o ./CLIProxyAPI ./cmd/server/

# ------------------------------------------------------------------
# Stage 2: runtime with CPA + mirasim plugin baked in.
# Render filesystem is ephemeral, so the .so MUST be inside the image,
# not mounted as a volume. We download the official release asset at
# build time so the 17MB .so never needs to be committed to git
# (your .gitignore ignores plugins/* and config.yaml anyway).
# ------------------------------------------------------------------
FROM debian:bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
  tzdata ca-certificates unzip curl \
  && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /CLIProxyAPI/plugins

COPY --from=builder /app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI
# Keep the full example as reference inside the image.
COPY --from=builder /app/config.example.yaml /CLIProxyAPI/config.example.yaml

# Render deploy config -> becomes the live config.yaml.
# NOTE: file is named render.config.yaml in git (config.yaml is gitignored),
# copied to /CLIProxyAPI/config.yaml inside the image.
COPY render.config.yaml /CLIProxyAPI/config.yaml

# Entrypoint rewrites `port:` from $PORT (Render injects PORT=10000 by
# default) so health checks pass no matter what port Render expects.
COPY deploy/entrypoint.sh /CLIProxyAPI/entrypoint.sh
RUN chmod +x /CLIProxyAPI/entrypoint.sh

# Fetch + verify the mirasim plugin for linux/amd64 (what Render runs).
# Download with the release asset's own filename so `sha256sum -c` finds
# it (checksums.txt lists files by name), then unpack into plugins/.
ARG MIRASIM_VERSION
RUN curl -fsSL -o "/tmp/mirasim_${MIRASIM_VERSION}_linux_amd64.zip" \
    "https://github.com/KIDA-MNESIA/cpa-plugin-mirasim/releases/download/v${MIRASIM_VERSION}/mirasim_${MIRASIM_VERSION}_linux_amd64.zip" \
  && curl -fsSL -o /tmp/checksums.txt \
    "https://github.com/KIDA-MNESIA/cpa-plugin-mirasim/releases/download/v${MIRASIM_VERSION}/checksums.txt" \
  && cd /tmp && grep "mirasim_${MIRASIM_VERSION}_linux_amd64.zip" checksums.txt | sha256sum -c - \
  && unzip -l "/tmp/mirasim_${MIRASIM_VERSION}_linux_amd64.zip" \
  && unzip -o "/tmp/mirasim_${MIRASIM_VERSION}_linux_amd64.zip" -d /CLIProxyAPI/plugins \
  && rm -f /tmp/mirasim_*.zip /tmp/checksums.txt /CLIProxyAPI/plugins/*.h \
  && ls -lh /CLIProxyAPI/plugins/ \
  && file /CLIProxyAPI/plugins/mirasim.so

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

ENTRYPOINT ["/CLIProxyAPI/entrypoint.sh"]
CMD ["/CLIProxyAPI/CLIProxyAPI", "-config", "/CLIProxyAPI/config.yaml"]
