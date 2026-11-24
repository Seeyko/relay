# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /relay ./cmd/relay

FROM gcr.io/distroless/static-debian12
COPY --from=build /relay /relay
ENV RELAY_DATA_DIR=/data RELAY_ADDR=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["/relay", "healthcheck"]
ENTRYPOINT ["/relay"]
