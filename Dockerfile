# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bin/cortex ./cmd/cortex

# Minimal runtime stage
FROM alpine:3.20

RUN apk --no-cache add ca-certificates git

COPY --from=builder /bin/cortex /usr/local/bin/cortex
RUN ln -s /usr/local/bin/cortex /usr/local/bin/repocortex

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/cortex"]
CMD ["serve"]
