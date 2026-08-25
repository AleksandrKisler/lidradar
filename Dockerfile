FROM golang:1.26-alpine AS build

ARG COMMAND=api
ARG VERSION=development
ARG REVISION=unknown
WORKDIR /src
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X lidradar/backend/platform/buildinfo.Version=${VERSION} -X lidradar/backend/platform/buildinfo.Revision=${REVISION}" \
    -o /out/lidradar "./backend/cmd/${COMMAND}"

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && addgroup -S lidradar && adduser -S -G lidradar lidradar
COPY --from=build /out/lidradar /usr/local/bin/lidradar
USER lidradar
ENTRYPOINT ["/usr/local/bin/lidradar"]
