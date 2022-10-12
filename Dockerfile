# build ui
FROM --platform=${BUILDPLATFORM} node:14-bullseye as ui-builder

#RUN apt-get update && apt-get install libvips-dev -y

RUN npm install -g gatsby-cli

RUN git clone https://github.com/keploy/ui

WORKDIR /ui

RUN npm install

RUN PATH_PREFIX='/' gatsby build --prefix-paths

# build stage
FROM --platform=${BUILDPLATFORM} golang:alpine as go-builder

RUN apk add -U --no-cache ca-certificates && apk add build-base

ENV GO111MODULE=on

# Build Delve
RUN go install github.com/go-delve/delve/cmd/dlv@latest

WORKDIR /app

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

COPY --from=ui-builder /ui/public /app/web/public

#RUN CGO_ENABLED=0 GOOS=linux go build -o health cmd/health/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -o keploy cmd/server/main.go

# final stage
FROM --platform=${BUILDPLATFORM} alpine
COPY --from=alpine /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
#COPY --from=builder /app/health /app/
COPY --from=go-builder /app/keploy /app/
COPY --from=go-builder /go/bin/dlv /

EXPOSE 6789
ENTRYPOINT ["/app/keploy"]