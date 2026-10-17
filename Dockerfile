FROM golang:1.27@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# APPLICATION_VERSION is passed by reusable-docker.yml (book000/templates)
# on every CI build; defaults to "dev" for a manual `docker build`.
ARG APPLICATION_VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X command-relay-mcp/internal/version.Version=${APPLICATION_VERSION}" -o /out/gateway ./gateway/cmd

FROM gcr.io/distroless/static-debian12:latest@sha256:6447365a6337c3732f412d1b74357b30a633831955b2bc45552b0086be907687
COPY --from=build /out/gateway /gateway
EXPOSE 8080
ENTRYPOINT ["/gateway"]
