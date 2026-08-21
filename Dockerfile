FROM golang:1.26@sha256:45a5f7a810238aabcbad211d70b9ae082022d96f7c7259e94041ad1b933575ac AS build
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
