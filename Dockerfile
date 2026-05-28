# syntax=docker/dockerfile:1.7
#
# Multi-stage build for secrets-bridge controller-manager. The runtime
# image is distroless/static so the final container has no shell, no
# package manager, and runs as a non-root UID by default.

FROM golang:1.26-alpine AS build
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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/manager /usr/local/bin/manager
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manager"]
