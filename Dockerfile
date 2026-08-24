FROM golang:1.26-alpine AS build

ARG COMMAND=api
WORKDIR /src
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lidradar "./backend/cmd/${COMMAND}"

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -S lidradar && adduser -S -G lidradar lidradar
COPY --from=build /out/lidradar /usr/local/bin/lidradar
USER lidradar
ENTRYPOINT ["/usr/local/bin/lidradar"]
