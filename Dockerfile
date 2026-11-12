# syntax=docker/dockerfile:1

# The build stage runs on the builder's own platform and cross-compiles, so no
# emulation is needed for the other architecture.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build

ARG TARGETOS
ARG TARGETARCH
# Shown in the page footer. The build context has no .git, so the toolchain
# cannot work them out by itself.
ARG VERSION
ARG COMMIT

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /gh-pr-lander .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /gh-pr-lander /gh-pr-lander

ENTRYPOINT ["/gh-pr-lander"]
