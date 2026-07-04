FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod puller.go puller_test.go ./
RUN go test ./... \
  && CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/puller .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates ffmpeg tini
WORKDIR /app
COPY --from=build /out/puller /app/puller

EXPOSE 18090/tcp

ENTRYPOINT ["/sbin/tini", "--", "/app/puller"]
