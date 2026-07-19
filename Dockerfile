FROM golang:1.25 AS builder
WORKDIR /go/src/github.com/OwO-Network/DLX
COPY . .
RUN go get -d -v ./
RUN CGO_ENABLED=0 go build -a -installsuffix cgo -o dlx .

FROM alpine:latest
WORKDIR /app
RUN addgroup -S dlx && adduser -h /app -G dlx -SH dlx
USER dlx:dlx
COPY --from=builder --chown=dlx:dlx /go/src/github.com/OwO-Network/DLX/dlx /app/dlx
EXPOSE 1188
ENTRYPOINT ["/app/dlx"]
