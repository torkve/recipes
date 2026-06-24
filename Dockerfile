# syntax=docker/dockerfile:1

# --- build stage ---
FROM golang:1.26-alpine AS build
WORKDIR /src
# Cache modules first.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Pure-Go build (no CGO) => a fully static binary; templates and static assets
# are embedded via go:embed, so the binary is self-contained.
RUN CGO_ENABLED=0 go build -ldflags='-s -w' -o /out/recipes ./cmd/recipes

# --- runtime stage ---
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -u 10001 app \
    && mkdir -p /data && chown app:app /data
COPY --from=build /out/recipes /usr/local/bin/recipes
USER app
ENV RECIPES_ADDR=:8080 \
    RECIPES_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["recipes"]
