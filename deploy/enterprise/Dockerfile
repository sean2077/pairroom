FROM golang:1.27.2-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/pairroom ./cmd/pairroom

FROM debian:bookworm-slim
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/pairroom /usr/local/bin/pairroom
RUN mkdir -p /var/lib/pairroom/backups /run/pairroom && chown -R 10001:10001 /var/lib/pairroom && chmod 0700 /var/lib/pairroom /var/lib/pairroom/backups
USER 10001:10001
WORKDIR /var/lib/pairroom
EXPOSE 8443
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/pairroom"]
CMD ["enterprise", "serve", "--config", "/run/pairroom/enterprise.json"]
