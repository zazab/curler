FROM golang:alpine

WORKDIR /go/src/curler

RUN apk --no-cache add git
RUN go get "github.com/docopt/docopt-go" && go get "github.com/kovetskiy/lorg"

VOLUME ["/app/results"]

ADD . /go/src/curler/
RUN go build -o /app/curler

FROM alpine:latest

ENTRYPOINT ["/app/curler"]

COPY --from=0 /app/curler /app/curler
