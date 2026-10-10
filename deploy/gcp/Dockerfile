# syntax=docker/dockerfile:1
#
# DRAFT - scoutanalytics public website for Cloud Run (read-only, GCS snapshot mode).
#
# Build from the REPO ROOT (the context must hold go.mod):
#   DOCKER_BUILDKIT=1 docker build -f deploy/gcp/Dockerfile -t scout-web:local .
# BuildKit reads deploy/gcp/Dockerfile.dockerignore (next to this file) instead of
# a .dockerignore at the context root. Never build from the prod working directory
# (it holds .env, scout.session.json and scoutanalytics_data/): use a clean clone.
#
# Dry check of what the build receives (README.md, step 6, "Before the first
# upload"): the context-list stage below copies the whole context, after the
# ignore file, and prints the file list. It is not part of the normal image
# (BuildKit skips stages the target does not need):
#   DOCKER_BUILDKIT=1 docker build -f deploy/gcp/Dockerfile --target=context-list \
#     --progress=plain --no-cache .
#
# The image holds no secrets and no configuration beyond safe defaults. It needs
# no database, no node and no Telegram: it reads the website snapshot from GCS
# (SCOUT_WEB_SOURCE=gcs, see deploy/gcp/INTERFACE.md). Until the Go side of that
# mode exists, this image builds but the container exits at start (it would look
# for a database).
#
# Pin the base images by digest before the first real deploy, e.g.
#   docker buildx imagetools inspect golang:1.24.11-bookworm
#   docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot
#   docker buildx imagetools inspect busybox:1.36.1
# and replace the tags below with name@sha256:<digest>.

ARG GO_VERSION=1.24.11

# ---- dry check only: list the build context ----------------------------------
FROM busybox:1.36.1 AS context-list
COPY . /ctx
RUN find /ctx -type f | sort && echo "files: $(find /ctx -type f | wc -l)"

# ---- build ------------------------------------------------------------------
FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src

# dependencies first (cached while go.mod/go.sum do not change)
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# only what the binary is built from: Go sources, the embedded page and schema
COPY *.go ./
COPY internal/ internal/
COPY frontend/ frontend/
COPY scoutanalytics.sql ./

# static binary: no cgo, no libc, runs on distroless/static
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w -buildid=" -o /out/scoutanalytics . \
 && go version -m /out/scoutanalytics | head -n 3

# ---- run --------------------------------------------------------------------
# distroless/static: CA certificates and tzdata, no shell, no package manager.
# The :nonroot tag runs as uid/gid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/scoutanalytics /scoutanalytics

# Safe defaults only. Bucket and object names come from the Cloud Run service
# (deploy/gcp/service.yaml); credentials from the runtime service account
# (metadata server), never from a file in the image. No listen address: in
# gcs mode the program listens on :$PORT, which Cloud Run sets (8080).
ENV SCOUT_WEB_SOURCE=gcs \
    SCOUT_WEB_EVENTS=off

WORKDIR /home/nonroot
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/scoutanalytics", "-web"]
