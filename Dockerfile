FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o reddit-gallery-dl .

FROM alpine:3.21
RUN addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=builder --chown=app:app /app/reddit-gallery-dl .
COPY --from=builder --chown=app:app /app/templates ./templates
COPY --from=builder --chown=app:app /app/static ./static
USER app
EXPOSE 5000
CMD ["./reddit-gallery-dl"]
