FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X github.com/xuhuanhello/gamefleet/internal/buildinfo.Version=${VERSION}" \
      -o /out/gamefleet ./cmd/gamefleet && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/xuhuanhello/gamefleet" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/gamefleet /gamefleet
# The deploy files of this version: the updater puts them in place with it.
COPY deploy /deploy
# A new volume mounted here takes this owner, so GameFleet can write its database.
COPY --from=build --chown=65532:65532 /out/data /var/lib/gamefleet
ENTRYPOINT ["/gamefleet"]
CMD ["-config", "/etc/gamefleet/config.json"]
