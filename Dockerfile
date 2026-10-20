# Build the web UI, then a static Go binary that embeds it. Everything that
# runs commands runs on the build machine (BUILDPLATFORM), so images for other
# architectures (linux/arm64) build without emulation.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm run build

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /corral ./cmd/corral

# The image's user, time zones and data folder, made here too (files only).
FROM --platform=$BUILDPLATFORM alpine:3.22 AS root
RUN apk add --no-cache tzdata && adduser -D -H -u 10001 corral && mkdir /data && chown corral /data

FROM alpine:3.22
COPY --from=root /etc/passwd /etc/group /etc/
COPY --from=root /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=root --chown=10001:10001 /data /data
COPY --from=build /corral /usr/local/bin/corral
USER corral
ENV CORRAL_LISTEN=0.0.0.0:8080 CORRAL_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
LABEL org.opencontainers.image.source=https://github.com/ihainan/corral org.opencontainers.image.licenses=Apache-2.0
ENTRYPOINT ["/usr/local/bin/corral"]
CMD ["serve"]
