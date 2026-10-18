# Build the web UI, then a static Go binary that embeds it.
FROM node:22-alpine AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /corral ./cmd/corral

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 10001 corral && mkdir /data && chown corral /data
COPY --from=build /corral /usr/local/bin/corral
USER corral
ENV CORRAL_LISTEN=0.0.0.0:8080 CORRAL_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/corral", "serve"]
