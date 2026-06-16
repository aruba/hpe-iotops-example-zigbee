FROM cgr.dev/chainguard/go:latest-dev AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/iot-app ./cmd/iot-app
RUN mkdir -p /out/data

FROM cgr.dev/chainguard/static:latest

WORKDIR /app
COPY --from=builder /out/iot-app /app/iot-app
COPY config.yaml /app/config.yaml
COPY --from=builder /out/data /home/app/data

ENV CONFIG_FILE=/app/config.yaml
EXPOSE 8080

ENTRYPOINT ["/app/iot-app"]