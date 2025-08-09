FROM golang:1.22 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/gofilemgr .

FROM gcr.io/distroless/static:nonroot
ENV USERNAME=admin \
    PASSWORD=changeme \
    ALLOWED_ROOTS=/data/sdd1,/data/hdd1
EXPOSE 8080
COPY --from=build /out/gofilemgr /gofilemgr
USER nonroot:nonroot
ENTRYPOINT ["/gofilemgr"]
