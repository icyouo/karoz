FROM golang:1.22-bookworm AS builder
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN go build -o /out/karoz ./cmd/karoz

FROM debian:bookworm-slim
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates git bash \
  && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /out/karoz /app/karoz
ENV KAROZ_ADDR=:8088
EXPOSE 8088
ENTRYPOINT ["/app/karoz"]
