# syntax=docker/dockerfile:1.7
#
# Multi-stage build for secrets-bridge controller-manager. The runtime
# image is distroless/static so the final container has no shell, no
# package manager, and runs as a non-root UID by default.

FROM golang:1.27.3-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG BUILD_VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
      -o /out/manager \
      ./cmd

FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/manager /usr/local/bin/manager
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manager"]
