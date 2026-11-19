FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
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
