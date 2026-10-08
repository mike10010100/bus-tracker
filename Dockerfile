# Multi-architecture Dockerfile for NJ Transit Bus Tracker Server
# Supports linux/amd64 (x86_64 PC/servers) and linux/arm64 (Raspberry Pi 3/4/5, Apple Silicon)
FROM python:3.11-slim

# Install system fonts so Pillow renders DejaVu Sans with pixel-perfection
RUN apt-get update && apt-get install -y --no-install-recommends \
    fonts-dejavu-core \
    tzdata \
    && rm -rf /var/lib/apt/lists/*

# Set default timezone to US Eastern for accurate schedule countdowns
ENV TZ=America/New_York
RUN ln -snf /usr/share/zoneinfo/$TZ /etc/localtime && echo $TZ > /etc/timezone

WORKDIR /app

# Install Python requirements
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy server and dashboard rendering application files
COPY bus_tracker.py render_dashboard.py server.py tracker-arm ./

# Expose HTTP port and Auto-Discovery UDP port
EXPOSE 8000/tcp
EXPOSE 8001/udp
EXPOSE 5353/udp

ENV PYTHONUNBUFFERED=1
ENV PORT=8000

CMD ["python3", "server.py"]
