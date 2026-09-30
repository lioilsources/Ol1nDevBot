# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ol1nbot ./cmd/ol1nbot \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ol1nbot /ol1nbot
COPY --from=build --chown=65532:65532 /out/data /data
ENV DB_PATH=/data/ol1nbot.db APPS_YAML=/config/apps.yaml TZ=Europe/Prague HTTP_ADDR=:8080
EXPOSE 8080
VOLUME /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s CMD ["/ol1nbot", "healthcheck"]
ENTRYPOINT ["/ol1nbot"]
