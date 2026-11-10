# Builds the small image that installs the bridge binaries into a volume:
#   /out/ffmpeg   the shim Jellyfin runs instead of ffmpeg
#   /out/ffprobe  link to the Jellyfin image's own ffprobe
#   /out/ffagent  the Linux agent, for running ffmpeg on another Linux machine
# The Jellyfin container itself stays the unmodified official image.
FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ENV CGO_ENABLED=0
RUN go build -buildvcs=false -trimpath -ldflags="-s -w -X main.shimVersion=${VERSION}" -o /out/ffshim ./cmd/ffshim \
 && go build -buildvcs=false -trimpath -ldflags="-s -w -X main.agentVersion=${VERSION}" -o /out/ffagent ./cmd/ffagent

FROM scratch
COPY --from=build /out/ffshim /out/ffagent /
ENTRYPOINT ["/ffshim", "install", "/out"]
