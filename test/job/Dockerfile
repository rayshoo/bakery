ARG BUILD_BASE_IMAGE_NAME=golang
ARG BUILD_BASE_IMAGE_TAG=1.25.4-alpine3.22
ARG BASE_IMAGE_NAME=alpine
ARG BASE_IMAGE_TAG=latest
ARG BUILDPLATFORM

FROM --platform=$BUILDPLATFORM $BUILD_BASE_IMAGE_NAME:$BUILD_BASE_IMAGE_TAG AS builder
ARG TARGETARCH
ARG VERSION=latest
WORKDIR /go/src
COPY go.mod ./
RUN go mod download && mkdir -p test/job
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
go build  \
-ldflags "-s -w -X main.version=$VERSION" \
-o build/job main.go

FROM $BASE_IMAGE_NAME:$BASE_IMAGE_TAG
LABEL maintainer="rayshoo"
COPY --from=builder /go/src/build/job /
RUN mv /job /app && chmod +x /app
ENTRYPOINT ["/app"]