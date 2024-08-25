FROM golang:latest

# Install necessary packages for building Neovim
RUN apt-get update && apt-get install -y \
    cmake \
    ninja-build \
    gettext \
    git \
    unzip \
    curl \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Set up the workspace
WORKDIR /workspace

# Set environment variables
ENV PATH="/go/bin:${PATH}"
ENV GOPATH="/go"
