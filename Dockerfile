# Build stage
FROM golang:1.23.4 AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG GIT_COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo \
    -ldflags "-X aspirant-online/server/data_functions.GitCommit=${GIT_COMMIT}" \
    -o main .

# Production stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /root/

ENV TZ=UTC
ENV PORT=8080

RUN mkdir -p /data/files/shared

COPY --from=builder /app/main .

EXPOSE 8080

CMD ["./main"]
