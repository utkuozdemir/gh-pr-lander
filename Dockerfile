# syntax=docker/dockerfile:1

# The build stage runs on the builder's own platform and cross-compiles, so no
# emulation is needed for the other architecture.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /sidero-rebaser .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /sidero-rebaser /sidero-rebaser

ENTRYPOINT ["/sidero-rebaser"]
