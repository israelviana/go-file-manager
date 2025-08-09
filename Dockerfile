# syntax=docker/dockerfile:1
FROM golang:1.22 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Multi-arch será tratado pelo CI (buildx). Localmente, ajuste GOARCH se necessário.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/gofilemgr .

FROM gcr.io/distroless/static:nonroot
# Defaults (sobrescreva em runtime via env ou secrets)
ENV USERNAME=admin \
    PASSWORD=changeme \
    ALLOWED_ROOTS=/data/sdd1,/data/hdd1
EXPOSE 8080
COPY --from=build /out/gofilemgr /gofilemgr
USER nonroot:nonroot
ENTRYPOINT ["/gofilemgr"]
