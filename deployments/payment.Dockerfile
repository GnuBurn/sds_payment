FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Cross-compiles natively, so `docker buildx build --platform linux/arm64`
# for the Raspberry Pi workers is fast (no QEMU for the Go build).
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/payment ./cmd/payment

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/payment /payment
EXPOSE 8080
ENTRYPOINT ["/payment"]
