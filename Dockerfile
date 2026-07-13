# Stage 1: build TypeScript frontend
FROM node:24-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package*.json ./
COPY frontend/scripts ./scripts
RUN npm ci
COPY frontend/ ./
RUN npm run build

# Stage 2: build Go binary (pure Go, no CGo/libopus — audio is relayed
# as opaque Opus payloads, never decoded or encoded server-side)
FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/frontend/dist ./frontend/dist
RUN CGO_ENABLED=0 go build -o bridge-server .

# Stage 3: minimal runtime image
FROM alpine:3.21
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/bridge-server /app/bridge-server
EXPOSE 8080
ENTRYPOINT ["/app/bridge-server"]
