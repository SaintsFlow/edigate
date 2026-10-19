FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/edigate ./cmd/edigate \
 && CGO_ENABLED=0 go build -trimpath -o /out/edigatectl ./cmd/edigatectl \
 && CGO_ENABLED=0 go build -trimpath -o /out/mock-onec ./mocks/onec \
 && CGO_ENABLED=0 go build -trimpath -o /out/mock-wms ./mocks/wms \
 && CGO_ENABLED=0 go build -trimpath -o /out/partner-emulator ./mocks/partner \
 && CGO_ENABLED=0 go build -trimpath -o /out/evil-validator ./servers/adversarial/evil-validator \
 && CGO_ENABLED=0 go build -trimpath -o /out/shadow-carrier ./servers/adversarial/shadow-carrier \
 && CGO_ENABLED=0 go build -trimpath -o /out/rugpull-rates ./servers/adversarial/rugpull-rates

# One image for the gateway and for the stand's helpers (mock-1c, mock-wms, partner-emulator, the
# three hostile servers, edigatectl for pinning inside the stand network); the compose file picks
# the binary.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/edigate /edigate
COPY --from=build /out/edigatectl /edigatectl
COPY --from=build /out/mock-onec /mock-onec
COPY --from=build /out/mock-wms /mock-wms
COPY --from=build /out/partner-emulator /partner-emulator
COPY --from=build /out/evil-validator /evil-validator
COPY --from=build /out/shadow-carrier /shadow-carrier
COPY --from=build /out/rugpull-rates /rugpull-rates
EXPOSE 8080
ENTRYPOINT ["/edigate"]
