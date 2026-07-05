FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /loadtest ./cmd/loadtest

FROM alpine:3.20
COPY --from=build /loadtest /usr/local/bin/loadtest
ENTRYPOINT ["loadtest"]
