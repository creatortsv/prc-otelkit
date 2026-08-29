# docker-build smoke for prc-otelkit.
#
# prc-otelkit is a Go library (no deployable service binary), so the smoke
# proves that the module compiles inside the pinned toolchain image with a
# pristine module cache — the same environment class CI and k8s build jobs
# use. No registry push, no runtime stage.
FROM golang:1.27.0@sha256:0ecdc2a9f6156af6451080bfe3d8382a662fcc4e209608c6f919e643453514c1 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build ./...
