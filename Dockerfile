FROM golang:1.27.4 AS build
ARG LD_FLAGS
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./...
RUN CGO_ENABLED=0 go build -trimpath -ldflags "$LD_FLAGS" -o /go/bin/app .
RUN mkdir /db && chown 65532:65532 /db

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /go/bin/app /app
COPY --from=build --chown=65532:65532 /db /db
EXPOSE 9098
VOLUME ["/db"]
ENTRYPOINT ["/app"]
