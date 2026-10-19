FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/edigate ./cmd/edigate \
 && CGO_ENABLED=0 go build -trimpath -o /out/mock-onec ./mocks/onec \
 && CGO_ENABLED=0 go build -trimpath -o /out/mock-wms ./mocks/wms \
 && CGO_ENABLED=0 go build -trimpath -o /out/partner-emulator ./mocks/partner

# One image for the gateway and for the stand's mocks (mock-1c, mock-wms, partner-emulator);
# the compose file picks the binary.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/edigate /edigate
COPY --from=build /out/mock-onec /mock-onec
COPY --from=build /out/mock-wms /mock-wms
COPY --from=build /out/partner-emulator /partner-emulator
EXPOSE 8080
ENTRYPOINT ["/edigate"]
