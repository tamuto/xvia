# Build xvia and the example dev server from the repository root:
#   docker build -f examples/docker-compose/Dockerfile .
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY examples/docker-compose/devserver ./examples/docker-compose/devserver
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/xvia ./cmd/xvia \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/devserver ./examples/docker-compose/devserver

FROM python:3.13-slim
COPY --from=build /out/xvia /out/devserver /usr/local/bin/
WORKDIR /srv
RUN echo '<h1>Hello from a container, served through xvia</h1>' > index.html
