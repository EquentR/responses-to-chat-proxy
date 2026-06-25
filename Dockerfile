FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -o /out/responses-to-chat-proxy .

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/responses-to-chat-proxy /usr/local/bin/responses-to-chat-proxy
EXPOSE 8000
ENTRYPOINT ["/usr/local/bin/responses-to-chat-proxy"]
