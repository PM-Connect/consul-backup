FROM golang:1.12 as base

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

FROM base as linux-scratch-amd64

RUN set -eux; \
    GOOS=linux CGO_ENABLED=0 GOGC=off GOARCH=amd64 go build -o consul-backup .; \
    chmod +x consul-backup

FROM alpine as certs

RUN apk add -U --no-cache ca-certificates

FROM scratch as production

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=linux-scratch-amd64 /app/consul-backup /

ENTRYPOINT ["/consul-backup"]