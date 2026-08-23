FROM golang:1.27@sha256:65b6f280bf050ec5af12716857e8ea8439d694dbba8f31ceeb7630670071f2bb AS build
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
