FROM golang:1.24 AS go

FROM node:18 AS node

# Install required tools
RUN apt-get update && apt-get install -y --no-install-recommends \
    make \
    curl \
    netcat-openbsd \
    postgresql-client \
    && rm -rf /var/lib/apt/lists/*

COPY --from=go /usr/local/go /usr/local/go
ENV GOPATH=/go
ENV CGO_ENABLED=0
ENV PATH=$GOPATH/bin:/usr/local/go/bin:$PATH

WORKDIR /app
CMD [ "sleep", "infinity" ]
