ARG go_version=1.12

FROM golang:${go_version} as base

WORKDIR /app

COPY go.mod go.sum ./

RUN set -eux; \
    go mod download; \
    go get -u golang.org/x/lint/golint

COPY . .

ENTRYPOINT ["go"]

FROM base as compiler

RUN set -eux; \
    GOOS=linux CGO_ENABLED=0 GOGC=off GOARCH=amd64 go build -o consul-backup .; \
    chmod +x consul-backup

FROM alpine as addons

RUN set -eux; \
    apk add -U --no-cache ca-certificates; \
    mkdir /scratch_tmp

FROM scratch as production

COPY --from=addons /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=addons /scratch_tmp /tmp
COPY --from=compiler /app/consul-backup /

ENTRYPOINT ["/consul-backup"]