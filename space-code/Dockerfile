# syntax=docker/dockerfile:1

# ---- build stage -----------------------------------------------------------
FROM golang:1.22-alpine AS build
WORKDIR /src

# Only stdlib is used, so there is nothing to download: builds work offline.
COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w -buildid=" -o /out/relayd ./cmd/relayd

# ---- runtime stage ---------------------------------------------------------
# alpine (not scratch) so we ship CA certificates for outbound HTTPS to the
# Hugging Face API, plus tzdata for correct UTC handling.
#
# /app/data and /app/rate hold the identity tree and the rate log. On a Hugging
# Face Space mount persistent storage at /app/data (and ideally /app/rate) or
# every restart starts from an empty bank of remembered identities.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 1000 relay \
 && adduser -D -u 1000 -G relay relay \
 && mkdir -p /app/data /app/bank /app/rate \
 && chown -R relay:relay /app
COPY --from=build /out/relayd /app/relayd
WORKDIR /app
USER 1000
ENV ADDR=:7860 \
    BANK_DIR=/app/bank \
    DATA_DIR=/app/data \
    RATE_DIR=/app/rate \
    TZ=UTC
EXPOSE 7860
HEALTHCHECK --interval=30s --timeout=3s --start-period=3s \
  CMD ["/app/relayd", "-healthcheck"]
ENTRYPOINT ["/app/relayd"]
