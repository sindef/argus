FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS build
ARG CMD=argus
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY web web
COPY LICENSE THIRD_PARTY_NOTICES ./
COPY third_party third_party
COPY scripts/update-notices.sh scripts/update-notices.sh
RUN sh scripts/update-notices.sh --check
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /app ./cmd/${CMD}

FROM build AS sources
RUN apt-get update && apt-get install -y --no-install-recommends jq && rm -rf /var/lib/apt/lists/*
COPY scripts/collect-app-sources.sh scripts/collect-app-sources.sh
RUN sh scripts/collect-app-sources.sh

FROM scratch AS source-bundle
COPY --from=sources /source-bundle/ /

FROM scratch AS bin
COPY --from=build /app /app
COPY LICENSE THIRD_PARTY_NOTICES /usr/share/doc/argus/
COPY third_party /usr/share/doc/argus/third_party/

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
LABEL org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.source="https://github.com/sindef/argus"
COPY LICENSE /LICENSE
COPY LICENSE THIRD_PARTY_NOTICES /usr/share/doc/argus/
COPY third_party /usr/share/doc/argus/third_party/
COPY --from=build /app /app
ENTRYPOINT ["/app"]
