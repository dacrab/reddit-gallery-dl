FROM golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o reddit-gallery-dl .

FROM alpine:3.24@sha256:3f4d6b32e26703c6082839234f43769d5046b3ac7c910033911d1e71d8cd630a
RUN addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=builder --chown=app:app /app/reddit-gallery-dl .
COPY --from=builder --chown=app:app /app/templates ./templates
COPY --from=builder --chown=app:app /app/static ./static
USER app
ENV GOMEMLIMIT=400MiB
EXPOSE 5000
CMD ["./reddit-gallery-dl"]
