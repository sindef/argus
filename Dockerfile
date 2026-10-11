FROM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS build
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

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
LABEL org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.source="https://github.com/sindef/argus"
COPY LICENSE /LICENSE
COPY --from=build /app /app
ENTRYPOINT ["/app"]
