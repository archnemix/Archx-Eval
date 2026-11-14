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
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 1000 relay \
 && adduser -D -u 1000 -G relay relay \
 && mkdir -p /app/data /app/bank \
 && chown -R relay:relay /app
COPY --from=build /out/relayd /app/relayd
WORKDIR /app
USER 1000
ENV ADDR=:7860 \
    BANK_DIR=/app/bank \
    DATA_DIR=/app/data \
    TZ=UTC
EXPOSE 7860
HEALTHCHECK --interval=30s --timeout=3s --start-period=3s \
  CMD ["/app/relayd", "-healthcheck"]
ENTRYPOINT ["/app/relayd"]
