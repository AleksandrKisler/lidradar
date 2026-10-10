# Образ одной команды: --build-arg COMMAND=api|worker|scheduler|migrate|ai-agent|
# platform-admin|ai-node-register|ai-node-manage. Выпуск собирает scripts/build-images.sh.
# Версия Go закреплена: плавающий тег 1.26 уже давал разные патчи (в 1.26.6 есть
# достижимые уязвимости stdlib, исправленные в 1.26.9). Меняйте её осознанно.
ARG GO_VERSION=1.26.9
FROM golang:${GO_VERSION}-alpine AS build

ARG COMMAND=api
ARG VERSION=development
ARG REVISION=unknown
WORKDIR /src
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Кэш сборки общий для команд: остальные образы выпуска компилируют только свой main.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X lidradar/backend/platform/buildinfo.Version=${VERSION} -X lidradar/backend/platform/buildinfo.Revision=${REVISION}" \
    -o /out/lidradar "./backend/cmd/${COMMAND}"

FROM alpine:3.22
ARG COMMAND=api
ARG VERSION=development
ARG REVISION=unknown
LABEL org.opencontainers.image.title="lidradar-${COMMAND}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
RUN apk add --no-cache ca-certificates tzdata && addgroup -S lidradar && adduser -S -G lidradar lidradar
COPY --from=build /out/lidradar /usr/local/bin/lidradar
USER lidradar
ENTRYPOINT ["/usr/local/bin/lidradar"]
