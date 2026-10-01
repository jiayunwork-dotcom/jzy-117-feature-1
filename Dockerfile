# Single-image build based on golang:1.22. The resulting image starts one
# container that listens on 8080; no UI, no external services. The SQLite
# driver (modernc.org/sqlite) is pure Go, so the binary stays static and
# cgo-free.
FROM golang:1.22 AS build
WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/openchannel ./cmd/server

FROM golang:1.22
WORKDIR /app
COPY --from=build /out/openchannel /app/openchannel

# Persistent state: a single SQLite file (plus its WAL) lives in /data, which
# is declared a volume so deployments can mount durable storage there.
RUN mkdir -m 0777 /data
ENV OPENCHANNEL_DATA=/data
VOLUME ["/data"]

EXPOSE 8080
USER nobody
ENTRYPOINT ["/app/openchannel"]
