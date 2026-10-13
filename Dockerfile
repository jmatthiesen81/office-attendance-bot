# syntax=docker/dockerfile:1

# ---- build -------------------------------------------------------------
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Pure-Go SQLite driver: no cgo, fully static binary.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/officebot ./cmd/officebot

# ---- runtime -----------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/officebot /officebot
USER nonroot:nonroot
# /data is expected as a bind mount (compose: ./volumes/officebot). No VOLUME
# instruction on purpose, so Docker never creates hidden anonymous volumes.
ENV DATABASE_PATH=/data/officebot.db HEALTH_FILE=/tmp/officebot.health
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 CMD ["/officebot", "healthcheck"]
ENTRYPOINT ["/officebot"]
CMD ["run"]
