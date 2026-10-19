FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/edigate ./cmd/edigate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/edigate /edigate
EXPOSE 8080
ENTRYPOINT ["/edigate"]
