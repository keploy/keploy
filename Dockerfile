# build stage
FROM golang:alpine as builder

RUN apk add -U --no-cache ca-certificates

ENV GO111MODULE=on

WORKDIR /app

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

#RUN CGO_ENABLED=0 GOOS=linux go build -o health cmd/health/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -o api cmd/api/main.go

# final stage
FROM scratch
COPY --from=alpine /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
#COPY --from=builder /app/health /app/
COPY --from=builder /app/api /app/

EXPOSE 8081
ENTRYPOINT ["/app/api"]