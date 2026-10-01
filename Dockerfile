# Single-image build based on golang:1.22. The resulting image starts one
# container that listens on 8080; no UI, no external services.
FROM golang:1.22 AS build
WORKDIR /src

# Cache dependencies first (there are none beyond the standard library, but
# keeping this order makes future adds cheap).
COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/openchannel ./cmd/server

FROM golang:1.22
WORKDIR /app
COPY --from=build /out/openchannel /app/openchannel

EXPOSE 8080
USER nobody
ENTRYPOINT ["/app/openchannel"]
