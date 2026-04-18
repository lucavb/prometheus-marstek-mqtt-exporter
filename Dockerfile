FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o marstek-exporter .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates wget

COPY --from=builder /app/marstek-exporter /marstek-exporter

EXPOSE 9734

ENV MARSTEK_LOG_FORMAT=json

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD wget -q --spider http://localhost:9734/health || exit 1

ENTRYPOINT ["/marstek-exporter"]
