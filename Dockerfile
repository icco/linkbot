# Build stage
FROM golang:1.27.1-alpine AS builder

ENV GOPROXY="https://proxy.golang.org"
ENV CGO_ENABLED=0

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /linkbot .

# Final stage
FROM alpine:3.24.2

LABEL org.opencontainers.image.source=https://github.com/icco/linkbot
LABEL org.opencontainers.image.description="Better links in your life"

RUN apk add --no-cache ca-certificates tzdata && adduser -S -u 1001 app

WORKDIR /app
COPY --from=builder --chown=app /linkbot .

USER app

EXPOSE 8080

ENTRYPOINT ["/app/linkbot"]
