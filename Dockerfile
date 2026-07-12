# Stage 1: build TypeScript frontend
FROM node:22-alpine AS frontend
WORKDIR /app/frontend
COPY frontend/package*.json ./
COPY frontend/scripts ./scripts
RUN npm ci
COPY frontend/ ./
RUN npm run build

# Stage 2: build Go binary (CGo enabled for libopus)
FROM golang:1.24-alpine AS builder
RUN apk add --no-cache gcc musl-dev opus-dev opusfile-dev libogg-dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/frontend/dist ./frontend/dist
RUN CGO_ENABLED=1 go build -o bridge-server .

# Stage 3: minimal runtime image
FROM alpine:3.21
RUN apk add --no-cache opus opusfile ca-certificates
WORKDIR /app
COPY --from=builder /app/bridge-server /app/bridge-server
EXPOSE 8080
ENTRYPOINT ["/app/bridge-server"]
