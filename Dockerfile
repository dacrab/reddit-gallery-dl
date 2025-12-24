FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o reddit-gallery-dl .

FROM alpine:latest
WORKDIR /root/
COPY --from=builder /app/reddit-gallery-dl .
COPY --from=builder /app/templates ./templates
COPY --from=builder /app/static ./static
ENV PORT=5000
EXPOSE 5000
CMD ["./reddit-gallery-dl"]