# İndirgen: the image carries only the app. The engines (yt-dlp, gallery-dl,
# ffmpeg, deno) are downloaded into the /data volume on first start and keep
# updating themselves there, so a new yt-dlp never needs a new image.

# Cross-compiles on the build machine's own platform: arm64 images build
# without emulation.
# golang:1-trixie is always the newest stable Go: release images get every
# standard-library security fix without editing this file.
FROM --platform=$BUILDPLATFORM golang:1-trixie AS build
WORKDIR /src
COPY go.mod ./
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=""
ARG COMMIT=""
# timetzdata: the runtime image has no tzdata, TZ still works.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -tags timetzdata \
      -ldflags "-s -w ${VERSION:+-X main.version=${VERSION#v}} -X main.commit=${COMMIT} -X main.buildDate=$(date -u +%Y-%m-%d)" \
      -o /out/indirgen .

# glibc is required: yt-dlp and deno ship glibc binaries (no Alpine/scratch).
# xz-utils unpacks the ffmpeg archive.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates xz-utils \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /data /downloads && chown 1000:1000 /data /downloads
COPY --from=build /out/indirgen /usr/local/bin/indirgen
# INDIRGEN_DOCKER=1 turns self-update off: update by pulling a new image.
ENV INDIRGEN_DOCKER=1 INDIRGEN_DATA=/data HOME=/data
USER 1000:1000
VOLUME ["/data", "/downloads"]
EXPOSE 8899
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 CMD ["indirgen", "health"]
ENTRYPOINT ["indirgen"]
CMD ["serve", "-listen", "0.0.0.0:8899", "-library", "/downloads"]
