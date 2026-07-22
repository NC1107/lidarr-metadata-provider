# Build the binary against the same Go version the module targets.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, so editing source does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=0.0.0-dev
# CGO stays off so the result is a static binary that runs on a scratch base.
# modernc.org/sqlite is pure Go precisely so this holds.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/lidarr-metadata-provider ./cmd/lidarr-metadata-provider && \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w" \
    -o /out/pipeline ./cmd/pipeline

FROM alpine:3.21

# ca-certificates is needed to fetch the dataset over HTTPS and, when the
# operator opts in, to reach MusicBrainz. tzdata keeps log timestamps sane.
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 1000 -h /data lidarr

# The pipeline ships alongside the server so nobody is forced to depend on
# prebuilt datasets. Anyone can point it at a MusicBrainz export and build
# their own; see docs/BUILDING.md.
COPY --from=build /out/lidarr-metadata-provider /usr/local/bin/
COPY --from=build /out/pipeline /usr/local/bin/lidarr-metadata-pipeline

# The dataset lives on a volume rather than in the image. Baking a
# multi-gigabyte file into a layer would make every release a full
# re-download, and would stop the data and the binary being updated
# independently.
VOLUME /data
WORKDIR /data
USER lidarr

EXPOSE 5001

ENV LMP_DATASET=/data/dataset.db

# Lidarr treats metadata failures as transient and retries, so an unhealthy
# container that keeps answering is worse than one that reports itself down.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:5001/ || exit 1

ENTRYPOINT ["lidarr-metadata-provider"]
CMD ["-addr", ":5001", "-dataset", "/data/dataset.db"]
