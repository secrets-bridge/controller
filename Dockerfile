# syntax=docker/dockerfile:1.7
#
# Multi-stage build for secrets-bridge controller-manager. The runtime
# image is distroless/static so the final container has no shell, no
# package manager, and runs as a non-root UID by default.

FROM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
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

FROM gcr.io/distroless/static:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
COPY --from=build /out/manager /usr/local/bin/manager
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/manager"]
