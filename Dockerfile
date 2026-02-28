FROM golang:1.24-bookworm
RUN sed -i 's/^Components: main$/Components: main non-free/' /etc/apt/sources.list.d/debian.sources && \
    apt-get update && apt-get install -y libfdk-aac-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /build
