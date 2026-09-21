# syntax=docker/dockerfile:1

# Build stage: matches go.mod's toolchain version exactly.
FROM golang:1.27.1-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
ARG VERSION=dev
ARG COMMIT=unknown
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/Ferousco-dev/mailx/internal/buildinfo.Version=${VERSION} -X github.com/Ferousco-dev/mailx/internal/buildinfo.Commit=${COMMIT}" -o /out/mailx ./cmd/mailx

# Runtime stage: alpine (not distroless) so the volume-owning /data
# directory can be created and chowned to the non-root user before the
# named volume mounts over it - Docker seeds a fresh named volume from
# the image's existing content/permissions at that path.
FROM alpine:3.20 AS runtime
# ca-certificates: outbound SMTP STARTTLS and HTTPS webhooks verify peers
# against the system roots; alpine ships none by default.
RUN apk add --no-cache ca-certificates \
    && addgroup -S mailx && adduser -S -G mailx mailx \
    && mkdir -p /data && chown mailx:mailx /data
WORKDIR /app
COPY --from=build /out/mailx /app/mailx
USER mailx:mailx
EXPOSE 2525 8080 9090
ENTRYPOINT ["/app/mailx"]
