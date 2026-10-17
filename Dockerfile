FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/ /
ENTRYPOINT ["/dockscale"]
CMD ["-config", "/etc/dockscale/dockscale.yaml"]
