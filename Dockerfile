FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY web web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /argus ./cmd/argus

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /argus /argus
ENTRYPOINT ["/argus"]
