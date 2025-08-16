# Build stage
FROM golang:1.21-alpine AS builder

# Install git and ca-certificates (needed for go mod download)
RUN apk add --no-cache git ca-certificates

# Set working directory
WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o mysql-auto-db-proxy main.go

# Final stage
FROM alpine:latest

# Install ca-certificates for HTTPS connections
RUN apk --no-cache add ca-certificates

# Create non-root user
RUN addgroup -g 1001 -S mysqlproxy && \
    adduser -u 1001 -S mysqlproxy -G mysqlproxy

# Set working directory
WORKDIR /app

# Copy binary from builder stage
COPY --from=builder /app/mysql-auto-db-proxy .

# Change ownership to non-root user
RUN chown mysqlproxy:mysqlproxy /app/mysql-auto-db-proxy

# Switch to non-root user
USER mysqlproxy

# Expose the proxy port
EXPOSE 3308

# Run the application
CMD ["./mysql-auto-db-proxy"]
