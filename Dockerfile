# Build
FROM golang:1.25-alpine AS builder

WORKDIR /app
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -o rustlog-proxy .

# Container
FROM scratch
WORKDIR /app
COPY --from=alpine:latest /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/rustlog-proxy .
EXPOSE 18080
ENTRYPOINT ["/app/rustlog-proxy"]
