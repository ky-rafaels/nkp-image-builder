# syntax=docker/dockerfile:1.4

# Scanners report the Go version and modules compiled into the binary, so keep
# GO_VERSION on a supported Go release (the newest two minors) at its latest
# patch, and go.mod's modules current (`make scan` checks the image).
ARG GO_VERSION=1.27.4

# The binary is cross-compiled (CGO off), so the build stage runs natively.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
# Build with this image's Go, never a toolchain go.mod asks to download.
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY web ./web
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/nkp-image-builder-webui .

# No shell or package manager: only CA certificates, tzdata and the nonroot
# user. Named by Debian release, so a rebuild picks up its fixes without
# moving to a new release unnoticed.
FROM gcr.io/distroless/static-debian13:nonroot
LABEL org.opencontainers.image.source=https://github.com/kubernetes-sigs/image-builder
COPY --from=build /out/nkp-image-builder-webui /nkp-image-builder-webui
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/nkp-image-builder-webui"]
