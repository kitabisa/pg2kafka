FROM scratch

WORKDIR /opt/pg2kafka
COPY ./bin/pg2kafka /opt/pg2kafka/
RUN chmod +x /opt/pg2kafka/pg2kafka

RUN apk --update --no-cache add git alpine-sdk bash
RUN wget -qO- https://github.com/edenhill/librdkafka/archive/v0.11.4-RC1.tar.gz | tar xz
RUN cd librdkafka-* && ./configure && make && make install

COPY sql ./sql

ENTRYPOINT ["/opt/pg2kafka/pg2kafka"]
