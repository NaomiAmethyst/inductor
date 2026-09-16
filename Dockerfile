# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.24 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X inductor/internal/inductor.Version=$VERSION" \
    -o /inductor ./cmd/inductor

FROM debian:bookworm-slim AS certificates
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

FROM scratch AS runtime-scratch
LABEL org.opencontainers.image.source="https://github.com/NaomiAmethyst/inductor" \
      org.opencontainers.image.licenses="GPL-3.0-only"
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /inductor /inductor
COPY LICENSE NOTICE /usr/share/doc/inductor/
COPY docs/third-party.md /usr/share/doc/inductor/docs/
COPY docs/licenses/ /usr/share/doc/inductor/docs/licenses/
WORKDIR /library
ENTRYPOINT ["/inductor"]
CMD ["--help"]

FROM certificates AS runtime-ffmpeg
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg openssh-client \
    && rm -rf /var/lib/apt/lists/*
LABEL org.opencontainers.image.source="https://github.com/NaomiAmethyst/inductor" \
      org.opencontainers.image.licenses="GPL-3.0-only"
COPY --from=build /inductor /inductor
COPY LICENSE NOTICE /usr/share/doc/inductor/
COPY docs/third-party.md /usr/share/doc/inductor/docs/
COPY docs/licenses/ /usr/share/doc/inductor/docs/licenses/
WORKDIR /library
ENTRYPOINT ["/inductor"]
CMD ["--help"]
