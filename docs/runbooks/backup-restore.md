# Резервное копирование и восстановление PostgreSQL

Логические копии снимаются `pg_dump -Fc` той же версии, что и сервер: по
умолчанию команда выполняется внутри контейнера compose `postgres`, чтобы
клиент и сервер не расходились по версии.

## Копия

```bash
scripts/backup.sh
```

Переменные: `LIDRADAR_BACKUP_DIR` (по умолчанию `backups/`),
`LIDRADAR_BACKUP_KEEP` (число хранимых копий, по умолчанию 14),
`LIDRADAR_BACKUP_MODE` — `compose` (по умолчанию), `container`
(`LIDRADAR_BACKUP_CONTAINER=<имя контейнера>`) или `local`
(`pg_dump` из PATH и `LIDRADAR_DATABASE_URL`). Копия меньше 1 КиБ считается
ошибкой. Каталог `backups/` исключён из git; в эксплуатации копии
переносятся во внешнее хранилище планировщиком хоста (cron/systemd timer),
например ежедневно в 03:00 UTC.

## Восстановление

```bash
scripts/restore.sh backups/lidradar-<штамп>.dump lidradar_restore
```

Целевая база пересоздаётся; восстановление идёт с `--exit-on-error` и без
владельцев и привилегий: роли `lidradar_app`, `lidradar_worker` и
`lidradar_platform` создаются миграцией `000020`, а права выдаются при
следующем запуске `cmd/migrate`. После восстановления в основную базу
обязателен `go run ./backend/cmd/migrate` и проверка `/health/ready`.

## Учение (LR-BE-2408)

```bash
scripts/restore-drill.sh
```

Снимает свежую копию, восстанавливает её в отдельную базу
`lidradar_restore_drill`, сравнивает число таблиц, последнюю миграцию и
счётчики `organizations`, `messages`, `risk_signals`, `revenue_events`,
`audit_log`, затем удаляет базу учения. Ненулевой код завершения означает,
что копия не воспроизводит данные. Учение входит в чек-лист пилота и
повторяется перед каждым релизом.
