# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
FROM golang:1.27-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/agentspool ./cmd/agentspool

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S agentspool-server \
    && adduser -S -D -H -G agentspool-server agentspool-server \
    && mkdir -p /var/lib/agentspool/server \
    && chown -R agentspool-server:agentspool-server /var/lib/agentspool
COPY --from=build /out/agentspool /usr/local/bin/agentspool
USER agentspool-server:agentspool-server
EXPOSE 8090
VOLUME ["/var/lib/agentspool/server"]
ENTRYPOINT ["/usr/local/bin/agentspool"]
CMD ["server"]
