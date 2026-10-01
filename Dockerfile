# Single-image build based on golang:1.22. The resulting image starts one
# container that listens on 8080; no UI, no external services. The data
# directory /data holds channel versions, jobs and results and is intended
# to be mounted as a persistent volume.
FROM golang:1.22 AS build
WORKDIR /src

# No third-party dependencies (the store is an embedded JSON-file store),
# but keep the module copy first so future adds are cached.
COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/openchannel ./cmd/server

FROM golang:1.22
WORKDIR /app
COPY --from=build /out/openchannel /app/openchannel

# Persistent data directory; the app creates channels/ and jobs/ under it.
RUN mkdir -p /data && chown nobody:nogroup /data
VOLUME ["/data"]
ENV OPENCHANNEL_DATA_DIR=/data

EXPOSE 8080
ENTRYPOINT ["/app/openchannel"]
