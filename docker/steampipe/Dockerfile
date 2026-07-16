# ---------------------------------------------------
# Stage 1: Build your custom AWS plugin
# ---------------------------------------------------
# Chainguard Go: glibc (Wolfi), no perl, Go 1.26.5. Official
# golang:*-bookworm images always pull in perl-base (Essential) and
# have no slim variant — Hub flags those OS CVEs on the FROM line.
# Pin by digest; public tags beyond :latest are limited.
FROM cgr.dev/chainguard/go:latest@sha256:0dfefd80cb9ce8db4be95d20d324c56005f40116524cdc8b8410f46ecd4732cb AS plugin-builder

WORKDIR /workspace

# Copy only go.mod and go.sum first for better layer caching
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the Steampipe plugin binary
RUN go build -tags "netgo" -o aws.plugin *.go

# ---------------------------------------------------
# Stage 2: Final image with Steampipe and your plugin
# ---------------------------------------------------
FROM ubuntu:22.04

# Install minimal runtime dependencies
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      unzip \
      sudo \
      gnupg && \
    rm -rf /var/lib/apt/lists/*

# Create a non-root 'steampipe' user
RUN useradd -ms /bin/bash steampipe && \
    echo 'steampipe ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers

USER steampipe
WORKDIR /home/steampipe
ENV HOME=/home/steampipe

# Install the Steampipe engine, pinned so image rebuilds are reproducible.
# Bump deliberately after checking plugin-protocol compatibility with the SDK
# version in go.mod (plugin-sdk v6 works with CLI v2.x).
RUN sudo /bin/sh -c "$(curl -fsSL https://steampipe.io/install/steampipe.sh)" -- v2.4.4

# Install the csv pluginm its used to testing purposes
RUN steampipe plugin install csv

# Install the Datadog plugin from Steampipe Hub
RUN steampipe plugin install datadog

# Define plugin installation path
ENV PLUGIN_DIR=/home/steampipe/.steampipe/plugins/hub.steampipe.io/plugins/turbot/aws@latest

# Create plugin directory
RUN mkdir -p $PLUGIN_DIR

# Copy the custom-built AWS plugin into the plugin directory
COPY --from=plugin-builder \
     /workspace/aws.plugin \
     $PLUGIN_DIR/aws.plugin

# Ensure the plugin binary is executable
RUN sudo chmod +x $PLUGIN_DIR/aws.plugin

# Create versions.json to register the plugin
RUN echo '{\
  "plugins": {\
    "hub.steampipe.io/plugins/turbot/aws@latest": {\
      "name": "aws",\
      "version": "0.1.0",\
      "struct_version": 20230502\
    }\
  },\
  "struct_version": 20220411\
}' > /home/steampipe/.steampipe/plugins/versions.json

# Launch Steampipe in the foreground
CMD ["steampipe", "service", "start", "--foreground"]
