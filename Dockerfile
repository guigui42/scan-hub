# syntax=docker/dockerfile:1
# scan-hub: static Go binary on distroless, non-root, read-only friendly.

FROM golang:1.27.3-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/scan-hub ./cmd/scan-hub \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG VERSION=dev
LABEL org.opencontainers.image.title="scan-hub" \
      org.opencontainers.image.description="ScanSnap (VENS) and eSCL/AirScan Wi-Fi scanning to Paperless-ngx" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.source="https://github.com/guigui42/scan-hub" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/scan-hub /usr/local/bin/scan-hub
# Owned by the nonroot user so a fresh named volume inherits writable ownership.
COPY --from=build --chown=65532:65532 /out/data /data
COPY LICENSE THIRD_PARTY_NOTICES /usr/share/doc/scan-hub/
USER 65532:65532
ENV SCANHUB_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8097
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/scan-hub", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/scan-hub"]
