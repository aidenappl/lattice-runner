FROM golang:1.24-alpine AS builder

WORKDIR /app

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o /lattice-runner .

# ---

FROM alpine:3.19

RUN apk add --no-cache ca-certificates curl docker-cli

RUN addgroup -S lattice && adduser -S lattice -G lattice -u 1001
# Runner needs access to Docker socket
RUN addgroup lattice docker 2>/dev/null || true
USER 1001:1001

COPY --from=builder /lattice-runner /usr/local/bin/lattice-runner

ENTRYPOINT ["lattice-runner"]
