# Stage 1: build TypeScript frontend
FROM node:22-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package*.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# Stage 2: build Go binary (CGo enabled for libopus)
FROM golang:1.22-bookworm AS builder
RUN apt-get update && apt-get install -y --no-install-recommends \
    libopus-dev libogg-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/frontend/dist ./frontend/dist
RUN CGO_ENABLED=1 go build -o bridge-server .

# Stage 3: minimal runtime image
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    libopus0 ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/bridge-server /app/bridge-server
EXPOSE 8080
ENTRYPOINT ["/app/bridge-server"]
