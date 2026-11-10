# syntax=docker/dockerfile:1
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# ==============================================================================
# Stage 1: Build the gemini-api CLI binary from source
# ==============================================================================
FROM golang:1.26-bookworm AS builder

ARG GEMINI_API_CLI_PKG="github.com/google-gemini/gemini-api-cli/cmd/gemini-api@main"

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOTOOLCHAIN=auto

RUN go install -ldflags="-s -w" "${GEMINI_API_CLI_PKG}"

# ==============================================================================
# Stage 2: Minimal hardened runtime image for self-hosted tool execution
# ==============================================================================
FROM python:3.12-slim-bookworm

RUN apt-get -o APT::Sandbox::User=root update \
  && apt-get -o APT::Sandbox::User=root install -y --no-install-recommends \
    bash \
    ca-certificates \
    curl \
    git \
    jq \
    procps \
    ripgrep \
  && rm -rf /var/lib/apt/lists/*

# Create an unprivileged user (UID/GID 1000) and isolated /workspace root
RUN groupadd --gid 1000 agent \
  && useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash agent \
  && mkdir -p /workspace \
  && (chown -R 1000:1000 /workspace /home/agent || true)

COPY --from=builder /go/bin/gemini-api /usr/local/bin/gemini-api

ENV HOME=/home/agent \
    PATH="/usr/local/bin:/usr/bin:/bin" \
    LANG=C.UTF-8

USER 1000:1000
WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gemini-api"]
CMD ["environments", "connect", "--root-dir", "/workspace", "--workdir", "/workspace"]
