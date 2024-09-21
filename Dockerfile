# Dockerfile for extractor-steampipe

FROM golang:1.22-alpine AS builder

# Install Git and wget for dependency management
RUN apk update && \
    apk add --no-cache git wget ca-certificates && \
    rm -rf /var/cache/apk/*

WORKDIR /app

# COPY all files and kick of build
COPY . .
RUN go build -o /app/aws.plugin ./

# Create necessary directories and set permissions for the non-root user
RUN mkdir -p /home/steampipe/.steampipe/plugins
COPY /app/aws.plugin /home/steampipe/.steampipe/plugins/local/aws/aws.plugin
