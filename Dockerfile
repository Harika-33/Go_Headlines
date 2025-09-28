# Stage 1: Build Go binary
FROM golang:1.25-alpine AS builder

# Install git for go module fetching
RUN apk add --no-cache git

WORKDIR /app

# Copy go.mod and go.sum first to leverage Docker cache
COPY go.mod go.sum ./  

RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the Go program
RUN go build -o newscli main.go

# Stage 2: Minimal runtime image
FROM alpine:latest

WORKDIR /app

# Copy the compiled binary
COPY --from=builder /app/newscli .

# Copy users.txt for input
COPY users.txt .

# Set environment variable placeholder for NewsAPI key
ENV NEWSAPI_KEY=""

# Run the program
CMD ["./newscli"]
