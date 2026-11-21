FROM golang:1.26.2-bookworm@sha256:6b9b1ff26b22fde9b31abc5c6994586f588107ee3aa54dba50626aaac5884995 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY schemas/ ./schemas/
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags='-s -w' -o /out/team-registry ./cmd/team-registry

FROM gcr.io/distroless/static-debian12:nonroot@sha256:52dcfbabb7457ea47c82f6e13af8c8a4a1d9f7b0145142b3ecab20f2b888411d
COPY --from=build /out/team-registry /team-registry
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/team-registry", "--config", "/etc/flowai/config.yaml"]
