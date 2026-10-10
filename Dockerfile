FROM golang:1.27.2 AS build
ARG CMD=argus
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY web web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /app ./cmd/${CMD}

FROM scratch AS bin
COPY --from=build /app /app

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /app /app
ENTRYPOINT ["/app"]
