# Stage 1: Build binaries
FROM golang:1.27.3-alpine AS builder

RUN apk add --no-cache protobuf-dev protoc git

WORKDIR /app

COPY go.mod go.sum* ./
RUN go mod download || true

COPY . .

RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@latest && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

RUN mkdir -p proto/taskv1 && \
    cp -f proto/task.proto proto/taskv1/task.proto 2>/dev/null || true && \
    protoc --go_out=. --go_opt=paths=source_relative \
           --go-grpc_out=. --go-grpc_opt=paths=source_relative \
           proto/taskv1/task.proto

RUN go mod tidy

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /bin/server server/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /bin/client client/main.go

FROM alpine:3.24
RUN apk add --no-cache ca-certificates

WORKDIR /root/
COPY --from=builder /bin/server .
COPY --from=builder /bin/client .

EXPOSE 50051 8443 8080

CMD ["./server"]