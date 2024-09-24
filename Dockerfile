# Stage 1: Build the Go binary
FROM golang:1.22-alpine AS builder

# Install Git and wget for dependency management
RUN apk update && \
    apk add --no-cache git wget ca-certificates && \
    rm -rf /var/cache/apk/*

WORKDIR /app

# Copy source code into the container
COPY . .

# Build the Go binary
RUN go build -o aws.plugin ./

# Stage 2: Prepare the final runtime image
FROM alpine:3.18

# Create necessary directories and set permissions for the non-root user
RUN mkdir -p /home/steampipe/.steampipe/plugins/local/aws

WORKDIR /home/steampipe

# Copy the built plugin from the builder stage
COPY --from=builder /app/aws.plugin /home/steampipe/.steampipe/plugins/local/aws

# Optionally run a check to ensure the file is present
RUN ls -la /home/steampipe/.steampipe/plugins/local/aws
