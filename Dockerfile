FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /argus ./cmd/argus

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /argus /argus
USER 25120:25120
EXPOSE 8443
ENTRYPOINT ["/argus"]
LABEL org.opencontainers.image.source="https://github.com/zekihan/argus" \
      org.opencontainers.image.licenses="AGPL-3.0" \
      org.opencontainers.image.title="argus" \
      org.opencontainers.image.description="Read-only detailed health checks for the homelab Kubernetes cluster and its hosts"
