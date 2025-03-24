ARG GO_VERSION="1.24"

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src
COPY go.* /src/
RUN go mod download
COPY . /src
RUN go build -o bin/gargle ./main.go

FROM gcr.io/distroless/static
WORKDIR /app
COPY --from=builder /src/bin/gargle /app/gargle
ENTRYPOINT ["/app/gargle"]
