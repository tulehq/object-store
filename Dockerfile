# syntax=docker/dockerfile:1

# The build stage runs on the builder's own platform and cross-compiles, so a
# multi-platform build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X tule.dev/object-store/internal/util/buildinfo.Version=${VERSION}" \
    -o /out/tule-object-store ./cmd \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source=https://github.com/tulehq/object-store \
      org.opencontainers.image.description="S3-compatible object store for local development and end-to-end tests" \
      org.opencontainers.image.licenses=MIT
COPY --from=build /out/tule-object-store /tule-object-store
COPY --from=build --chown=nonroot:nonroot /out/data /data

# TULE_Auth_AccessKeyID and TULE_Auth_SecretAccessKey must be set at run time;
# every request is verified against them. TULE_Server_Domain enables
# <bucket>.<domain> virtual-hosted-style requests.
ENV TULE_GasEnv=development \
    TULE_Storage_RootDir=/data \
    TULE_Server_Domain=localhost

EXPOSE 9000
VOLUME /data
ENTRYPOINT ["/tule-object-store"]
