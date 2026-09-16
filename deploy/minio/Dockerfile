# Last MinIO community security release (GHSA-jjjj-jwhf-8rgr).
# Docker Hub no longer serves minio/minio; community builds are source-only.
ARG MINIO_RELEASE=RELEASE.2025-10-15T17-29-55Z
ARG GO_VERSION=1.24.8

FROM golang:${GO_VERSION} AS build
ARG MINIO_RELEASE
WORKDIR /src
RUN git clone --depth 1 --branch "${MINIO_RELEASE}" https://github.com/minio/minio.git .
ENV CGO_ENABLED=0
RUN mkdir -p /out \
 && go build -tags kqueue -trimpath \
      --ldflags "$(go run buildscripts/gen-ldflags.go)" \
      -o /out/minio .

# Keep official entrypoint, curl, and mc so existing healthchecks work.
FROM quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z
COPY --from=build /out/minio /usr/bin/minio
