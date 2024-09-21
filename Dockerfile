# Dockerfile for extractor-steampipe

# Stage 1: Build the Go application
FROM golang:1.21-alpine AS builder

# Install Git and wget for dependency management
RUN apk update && \
    apk add --no-cache git wget ca-certificates && \
    rm -rf /var/cache/apk/*

WORKDIR /app

# Copy necessary files and modules and build the application
COPY .. .
RUN go build -o /app/aws.plugin ./

# Create necessary directories and set permissions for the non-root user
RUN mkdir -p /home/steampipe/.steampipe/plugins
COPY app/aws.plugin /home/steampipe/.steamipe/plugins/aws/aws.plugin
