FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o reddit-gallery-dl .

FROM alpine:3.21
RUN addgroup -S app && adduser -S -G app app
WORKDIR /home/app
COPY --from=builder /app/reddit-gallery-dl .
COPY --from=builder /app/templates ./templates
COPY --from=builder /app/static ./static
RUN chown -R app:app /home/app
USER app
ENV PORT=5000
EXPOSE 5000
CMD ["./reddit-gallery-dl"]