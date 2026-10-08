# Multi-architecture Dockerfile for Transit Tracker Server
# Supports linux/amd64 (x86_64 PC/servers) and linux/arm64 (Raspberry Pi 3/4/5, Apple Silicon)

# Stage 1: Compile static Kindle ARM client (tracker-arm)
FROM golang:alpine AS builder
WORKDIR /build
COPY client-go/ .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o /tracker-arm .

# Stage 2: Runtime image
FROM python:3.11-slim

# Install system fonts and curl for container health checks
RUN apt-get update && apt-get install -y --no-install-recommends \
    fonts-dejavu-core \
    tzdata \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Set default timezone to US Eastern for accurate schedule countdowns
ENV TZ=America/New_York
RUN ln -snf /usr/share/zoneinfo/$TZ /etc/localtime && echo $TZ > /etc/timezone

WORKDIR /app

# Install Python requirements
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy server and dashboard rendering application files
COPY bus_tracker.py citibike.py render_dashboard.py server.py ./

# Copy compiled static Kindle ARM client from builder stage
COPY --from=builder /tracker-arm ./tracker-arm

# Expose HTTP port and Auto-Discovery UDP port
EXPOSE 8000/tcp
EXPOSE 8001/udp
EXPOSE 5353/udp

ENV PYTHONUNBUFFERED=1
ENV PORT=8000

CMD ["python3", "server.py"]
