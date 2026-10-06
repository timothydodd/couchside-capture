# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/capture ./cmd/capture

FROM alpine:3.21
# Chromium draws the page, ffmpeg encodes it. The fonts are for pages that
# don't bring their own.
RUN apk add --no-cache chromium ffmpeg font-noto font-noto-emoji ca-certificates tzdata \
 && adduser -D -H -u 1000 capture \
 && mkdir -p /tmp/couchside-capture && chown capture:capture /tmp/couchside-capture
COPY LICENSE /usr/share/licenses/couchside-capture/
COPY --from=build /out/capture /usr/local/bin/capture

ENV CAPTURE_ADDR=:9800 \
    CAPTURE_CHROME=/usr/bin/chromium-browser \
    CAPTURE_DIR=/tmp/couchside-capture \
    HOME=/tmp/couchside-capture \
    XDG_CONFIG_HOME=/tmp/couchside-capture \
    XDG_CACHE_HOME=/tmp/couchside-capture

USER 1000:1000
EXPOSE 9800
ENTRYPOINT ["capture"]
