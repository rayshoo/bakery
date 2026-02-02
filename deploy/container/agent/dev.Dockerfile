ARG BUILD_BASE_IMAGE_NAME=golang
ARG BUILD_BASE_IMAGE_TAG=1.25.5-alpine3.23

ARG BUILDPLATFORM
FROM --platform=$BUILDPLATFORM $BUILD_BASE_IMAGE_NAME:$BUILD_BASE_IMAGE_TAG AS builder
ARG TARGETARCH
ARG VERSION=latest
WORKDIR /go/src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
go build  \
-ldflags "-s -w -X main.version=$VERSION" \
-o build/app cmd/agent/main.go

FROM gcr.io/kaniko-project/executor:v1.24.0-debug
LABEL maintainer="rayshoo"
COPY --from=builder /go/src/build/app /busybox/bakery-dev
ENTRYPOINT ["/busybox/bakery-dev"]