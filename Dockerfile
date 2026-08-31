ARG BASE_IMAGE=registry.access.redhat.com/ubi9-micro:latest


FROM registry.access.redhat.com/ubi9/go-toolset:9.8-1787774815 AS builder

ARG APP_VERSION="0.0.0-dev"
# Install make as root (UBI9 go-toolset doesn't include it), then switch back to non-root.
USER root
RUN dnf install -y make && dnf clean all
WORKDIR /build
RUN chown 1001:0 /build
USER 1001

ENV GOBIN=/build/.gobin
RUN mkdir -p $GOBIN
ENV PATH="${GOBIN}:${PATH}"

COPY --chown=1001:0 go.mod go.sum ./
RUN --mount=type=cache,target=/opt/app-root/src/go/pkg/mod,uid=1001 \
    go mod download

COPY --chown=1001:0 . .

# For FIPS-compliant builds, use CGO_ENABLED=1 + GOEXPERIMENT=boringcrypto.
RUN --mount=type=cache,target=/opt/app-root/src/go/pkg/mod,uid=1001 \
    --mount=type=cache,target=/opt/app-root/src/.cache/go-build,uid=1001 \
    GOOS=linux make build

# Runtime stage
FROM ${BASE_IMAGE}

WORKDIR /app

# ubi9-micro doesn't include CA certificates; copy from builder for TLS (e.g. Google Pub/Sub)
COPY --from=builder /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem
COPY --from=builder /build/bin/hyperfleet-applier /app/hyperfleet-applier
COPY --from=builder /build/LICENSE /licenses/LICENSE

USER 65532:65532

ENTRYPOINT ["/app/hyperfleet-applier"]

ARG APP_VERSION="0.0.0-dev"

LABEL name="hyperfleet-applier" \
      vendor="Red Hat, Inc." \
      version="${APP_VERSION}" \
      summary="HyperFleet Applier" \
      description="Controller responsible for Hyperfleet ReadDesires, ApplyDesires and DeleteDesires"
