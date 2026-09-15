FROM golang:1.24 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /inductor ./cmd/inductor

FROM scratch
COPY --from=build /inductor /inductor
ENTRYPOINT ["/inductor"]
CMD ["--help"]
