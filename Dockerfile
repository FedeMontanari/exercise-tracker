# ---- Build stage -----------------------------------------------------------
# --platform=$BUILDPLATFORM runs the compiler natively on the build machine;
# Go then cross-compiles for the target (e.g. a Raspberry Pi). Since the
# SQLite driver is pure Go, CGO is disabled and no C toolchain is needed.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# TARGETVARIANT is "v7" on 32-bit Raspberry Pi OS; GOARM wants just "7".
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w" -o /out/exercise-logger .

# ---- Runtime stage ---------------------------------------------------------
# The binary embeds templates, CSS and timezone data, so an empty image works.
FROM scratch
COPY --from=build /out/exercise-logger /exercise-logger
ENV ADDR=:8080 DB_PATH=/data/logger.db
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/exercise-logger"]
