# syntax=docker/dockerfile:1.4
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY web ./web
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/nkp-image-builder-webui .

FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.source=https://github.com/kubernetes-sigs/image-builder
COPY --from=build /out/nkp-image-builder-webui /nkp-image-builder-webui
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/nkp-image-builder-webui"]
