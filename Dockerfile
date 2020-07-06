FROM golang:alpine as builder

WORKDIR /go/src/github.com/blendle/pg2kafka
ADD . ./

RUN apk --update --no-cache add git alpine-sdk bash
RUN wget -qO- https://github.com/edenhill/librdkafka/archive/v0.11.4-RC1.tar.gz | tar xz
RUN cd librdkafka-* && ./configure && make && make install

FROM scratch
LABEL maintainer="Jurre Stender <jurre@blendle.com>"
COPY sql ./sql
COPY ./bin/pg2kafka /pg2kafka
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
CMD ["/pg2kafka"]
