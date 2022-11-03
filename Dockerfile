FROM golang:1.9.4-alpine3.6

MAINTAINER Minio Inc <dev@minio.io>

ENV GOPATH /go
ENV PATH $PATH:$GOPATH/bin
ENV CGO_ENABLED 0
ENV MINIO_UPDATE off
ENV MINIO_ACCESS_KEY_FILE=access_key \
    MINIO_SECRET_KEY_FILE=secret_key \
    MINIO_KMS_MASTER_KEY_FILE=kms_master_key \
    MINIO_SSE_MASTER_KEY_FILE=sse_master_key \
    MINIO_CONFIG_ENV_FILE=config_env_file \
    MINIO_SITE_NAME=site_name \
    MINIO_SITE_REGION=site_region \
    MINIO_SERVER_URL=server_url 

WORKDIR /go/src/github.com/avdhoot505/minio/

COPY dockerscripts/docker-entrypoint.sh /usr/bin/

RUN  \
     apk add --no-cache ca-certificates curl && \
     apk add --no-cache --virtual .build-deps git && \
     echo 'hosts: files mdns4_minimal [NOTFOUND=return] dns mdns4' >> /etc/nsswitch.conf && \
     go get -v -d github.com/avdhoot505/minio && \
     cd /go/src/github.com/avdhoot505/minio && \
     go install -v -ldflags "$(go run buildscripts/gen-ldflags.go)" && \
     rm -rf /go/pkg /go/src /usr/local/go && apk del .build-deps

EXPOSE 9000

ENTRYPOINT ["/usr/bin/docker-entrypoint.sh"]

VOLUME ["/data"]

CMD ["minio"]
