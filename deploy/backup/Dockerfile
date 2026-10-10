# Образ копий вне хоста (ADR 0051): pg_dump, age и rclone. Контекст сборки — корень
# репозитория (нужен scripts/backup.sh), собирает образ scripts/build-images.sh.
#
# Клиент PostgreSQL обязан быть не старше сервера: когда база перейдёт на новую
# мажорную версию, поднимите версию пакета здесь, иначе pg_dump откажется работать
# и копии перестанут создаваться (молча неполной копии он не создаёт).
FROM alpine:3.24

ARG VERSION=development
ARG REVISION=unknown
LABEL org.opencontainers.image.title="lidradar-backup" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

RUN apk add --no-cache bash postgresql18-client age rclone jq curl ca-certificates tzdata \
    && addgroup -S -g 10001 backup \
    && adduser -S -u 10001 -G backup -H -h /tmp backup \
    && install -d -m 700 -o backup -g backup /spool

COPY scripts/backup.sh deploy/backup/lib.sh deploy/backup/lidradar-backup deploy/backup/lidradar-backup-cycle /usr/local/lib/lidradar/
ENV PATH="/usr/local/lib/lidradar:${PATH}"

USER backup
STOPSIGNAL SIGTERM
ENTRYPOINT ["lidradar-backup"]
CMD ["run"]
