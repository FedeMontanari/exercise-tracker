# ---- Build stage -----------------------------------------------------------
# Works with both Docker builders:
#   - the classic builder (what Portainer's "Build image" uses), which builds
#     for the machine it runs on, e.g. natively on the Raspberry Pi;
#   - BuildKit/buildx (`docker compose build`, `docker buildx build
#     --platform linux/arm64`), which fills in the TARGET* args below.
# Since the SQLite driver is pure Go, CGO is disabled and no C toolchain is needed.
FROM golang:1.26-alpine AS build

# Set by BuildKit, empty with the classic builder. Go treats an empty
# GOOS/GOARCH/GOARM as "use this machine's", so both cases work.
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# TARGETVARIANT is "v7" on 32-bit Raspberry Pi OS; GOARM wants just "7".
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" GOARM="${TARGETVARIANT#v}" \
    go build -trimpath -ldflags="-s -w" -o /out/exercise-logger .

# ---- Runtime stage ---------------------------------------------------------
# The binary embeds templates, CSS and timezone data, so an empty image works.
FROM scratch
COPY --from=build /out/exercise-logger /exercise-logger
ENV ADDR=:8080 DB_PATH=/data/logger.db
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/exercise-logger"]
