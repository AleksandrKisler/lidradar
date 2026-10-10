# Развёртывание production-стека на одном хосте

Редакция 2026-10-10, [ADR 0050](../adr/0050-production-topology.md). Стек описан
в [`deploy/production/compose.yaml`](../../deploy/production/compose.yaml) и
[`Caddyfile`](../../deploy/production/Caddyfile). Процедура проверена на Docker
Desktop (Docker Engine 28.1.1, Compose 2.35.1, macOS, arm64) с доменом `localhost`;
на Linux-хосте с настоящим доменом и сертификатом она не выполнялась, поэтому
RG-DEPLOY не закрыт (см. «Что не закрыто»).

```
интернет ─ 80, 443 ─▶ edge (Caddy) ─▶ web   (статика веб-клиента)
                         │
                         └──────────▶ api ─────▶ postgres
                                      worker ──▶ postgres      сеть data: без выхода наружу
                                      scheduler ▶ postgres     (api и worker выходят к Telegram
                                      backup ──▶ postgres       через сети edge и egress)
                                         └─ шифрованная копия каждые 10 минут ─▶ хранилище вне хоста
```

Наружу открыты только порты `80` и `443`. Остальное доступно внутри сетей Docker.

## Требования

- Linux-хост с Docker Engine и плагином Compose v2 (проверено на 28.1.1 и 2.35.1,
  более старые версии не проверялись). Пользователи группы `docker` фактически
  root на хосте: включайте в неё только операторов.
- Не менее 2 vCPU, 4 ГБ памяти и 20 ГБ диска под тома Docker: пределы памяти
  контейнеров в сумме около 2,8 ГБ вместе с миграцией, в простое на пустой базе
  стек занимает около 170 МиБ (замер 2026-10-10: postgres 98, edge 32, api 14,
  worker 12, scheduler 12, web 4). Ёмкость под нагрузкой —
  [10-operations.md](../backend/10-operations.md) § 6. Резервные копии хранить
  на другом диске или хосте.
- Свободные порты `80` и `443`, доступные из интернета: через них Caddy выпускает
  и продлевает сертификат. Другие порты не публикуются; Docker обходит `ufw` и
  `firewalld` для опубликованных портов, поэтому не добавляйте `ports:` в файл.
- DNS-записи `A` (и `AAAA`, см. «Проверка адреса клиента») домена указывают на хост
  до первого запуска. Синхронизированные часы (`chrony` или `systemd-timesyncd`).
- Выход хоста к Docker Hub (`caddy`, `postgres`), к центру сертификации и, из
  контейнеров `api` и `worker`, к `api.telegram.org`.
- Токен бота Telegram для уведомлений и имя бота без `@`.

## 1. Сборка и доставка выпуска

На машине сборки, из чистого дерева нужного коммита:

```bash
scripts/build-images.sh --web-dir ../lidradar-web
```

Скрипт отказывает на изменённом дереве (в бэкенде и в клиенте), собирает
`lidradar-<команда>:<тег>` и `lidradar-web:<тег>` и печатает `LIDRADAR_VERSION=<тег>`.
Тег — первые 12 символов git sha. Образ `lidradar-ai-agent` нужен только узлу AI (`LIDRADAR_AI_AGENT_IMAGE` в `docker-compose.ai.yml`).
Доставка без реестра:

```bash
TAG=<тег>
docker save $(for c in api worker scheduler migrate backup platform-admin ai-node-register ai-node-manage web; do echo lidradar-$c:$TAG; done) \
  | gzip | ssh deploy@host 'gunzip | docker load'
git archive <коммит> deploy scripts | ssh deploy@host 'mkdir -p /opt/lidradar && tar -x -C /opt/lidradar'
```

Файлы `deploy/` и `scripts/` на хосте должны быть того же коммита, что и образы.

## 2. Окружение

```bash
cd /opt/lidradar/deploy/production
cp .env.example .env && chmod 600 .env
openssl rand -hex 24      # POSTGRES_PASSWORD (администратор postgres)
openssl rand -hex 24      # LIDRADAR_DB_OWNER_PASSWORD (владелец базы lidradar)
openssl rand -hex 24      # LIDRADAR_DB_RUNTIME_PASSWORD (рабочий логин lidradar_runtime)
openssl rand -base64 32   # LIDRADAR_INTEGRATION_ENCRYPTION_KEY
```

Заполните `.env` редактором (значения не вводить в командной строке: они попадут в
историю). Обязательны `LIDRADAR_VERSION`, `LIDRADAR_DOMAIN`, `LIDRADAR_ACME_EMAIL`,
`POSTGRES_PASSWORD`, `LIDRADAR_DB_OWNER_PASSWORD`, `LIDRADAR_DB_RUNTIME_PASSWORD` (три
разных пароля базы, [раздел 11](#11-роли-базы-данных)), `LIDRADAR_INTEGRATION_ENCRYPTION_KEY`, `LIDRADAR_TELEGRAM_TOKEN`,
`LIDRADAR_TELEGRAM_BOT_USERNAME`, `LIDRADAR_DATABASE_ALLOW_PLAINTEXT=true` для
встроенной базы (решение оператора, ADR 0050) и настройки копий вне хоста:
`LIDRADAR_BACKUP_REMOTE`, `LIDRADAR_BACKUP_S3_*`, `LIDRADAR_BACKUP_AGE_RECIPIENTS`
(подготовка хранилища и ключей — [offhost-backup.md](offhost-backup.md) § 1: без неё стек не
запустится). Остальное перечислено и объяснено в `.env.example`. **Сохраните копию `LIDRADAR_INTEGRATION_ENCRYPTION_KEY` отдельно
от копий базы**: без ключа сохранённые токены каналов не расшифровать, а генерация
нового ключа его не заменяет. Пароли базы применяются один раз, когда создаётся том:
правка значения в `.env` позже пароль в базе не меняет, порядок смены описан в
[разделе 11](#11-роли-базы-данных). Для `staging` задайте `LIDRADAR_ENV=staging` и
отдельный хост, домен и данные.

```bash
docker compose config --quiet && echo 'конфигурация корректна'
```

При пропущенном значении команда называет переменную. Содержимое `.env` не
выводите: `docker compose config` без `--quiet` печатает секреты.

## 3. Первый запуск

```bash
docker compose up -d
docker compose ps
```

Ожидается: `migrate` завершён с кодом 0, `edge`, `web`, `api`, `postgres` — `healthy`,
`worker` и `scheduler` — `running`, `backup` — `healthy` после первой подтверждённой копии
(перед запуском проверьте настройку `docker compose run --rm backup check`). Выпуск сертификата виден в журнале Caddy:

```bash
docker compose logs edge | grep 'certificate obtained'
```

Если сертификата нет, проверьте DNS, порты `80` и `443` снаружи и журнал `edge`
(см. «Диагностика»). При повторных неудачах центр сертификации ограничивает число
попыток; том `caddy-data` с ключами и сертификатами без нужды не удаляйте.

## 4. Первый администратор, Telegram и AI-узел

1. Зарегистрируйтесь в веб-клиенте по адресу `https://<домен>`.
2. Выдайте право платформенного администратора и проверьте:

   ```bash
   docker compose run --rm platform-admin grant --email owner@example.com --note "первый администратор"
   docker compose run --rm platform-admin list
   ```

3. Токен бота владельца клиента подключается в веб-клиенте (раздел «Интеграции»,
   ADR 0045); адрес вебхука строится из `LIDRADAR_PUBLIC_BASE_URL`, который
   compose выводит из домена.
4. Узел AI: реквизиты создаёт `ai-node-register`; файл попадает на хост с правами
   `0600` пользователя, который запустил команду:

   ```bash
   install -d -m 700 secrets
   docker compose run --rm --user "$(id -u):$(id -g)" -v "$PWD/secrets:/out" \
     ai-node-register --tenant-id <uuid организации> --name home-gpu --output /out/ai-node.json
   ```

   Передайте файл на узел по защищённому каналу и удалите его с хоста. Управление
   узлом: `docker compose run --rm ai-node-manage allow-tenant|rotate|revoke …`.

## 5. Приёмка

С другого компьютера (проверяет то, что видит посторонний, включая закрытые порты):

```bash
scripts/smoke-production.sh https://<домен>
```

На хосте, дополнительно с версией сборки и миграциями изнутри контейнера:

```bash
cd /opt/lidradar
scripts/smoke-production.sh https://<домен> --closed-ports '' \
  --version <тег> --compose-dir deploy/production
```

Для стека с другим именем проекта или файлом окружения задайте `COMPOSE_PROJECT_NAME`
и `COMPOSE_ENV_FILES`, как для `docker compose`. `--insecure` принимает любой
сертификат и нужен только для локальной пробы. Скрипт ничего не создаёт и не
меняет; код `0` значит «замечаний нет».

### Проверка адреса клиента (обязательна на настоящем хосте)

API должен видеть адрес клиента, а не адрес edge или шлюза Docker, иначе все
пользователи делят пределы частоты (ADR 0049). Войдите в веб-клиент с компьютера,
чей публичный адрес вам известен, и сравните:

```bash
docker compose exec -T postgres psql -U lidradar -d lidradar -At \
  -c 'select ip, created_at from sessions order by created_at desc limit 3'
```

В `ip` должен стоять ваш публичный адрес. Адрес шлюза подсети edge (по умолчанию
`172.29.88.1`) или самого edge (`LIDRADAR_EDGE_IP`) значит, что источник потерян по пути
(у Docker это бывает при проксировании опубликованных портов через userland-proxy,
например для IPv6); не открывайте пользователям доступ, пока это не исправлено.
Проверьте это для IPv4 и, если в DNS есть `AAAA`, для IPv6. На Docker Desktop вы
увидите адрес шлюза виртуальной машины: это его особенность, а не показатель Linux-хоста.

Затем вручную: вход, создание организации, обновление риска в реальном времени
(индикатор в шапке), подключение Telegram.

## 6. Обновление выпуска

Недоступность API — около 10 секунд на пустой базе без новых миграций (измерено),
статика отвечает всё время. Миграции идут только вперёд, а старая сборка с новой
схемой отвечает `503` на `/health/ready`, поэтому приложение останавливают целиком.

1. Соберите и доставьте новый выпуск и файлы `deploy/` и `scripts/` (раздел 1).
2. **Снимите копию** перед обновлением и убедитесь, что файл создан. Служба `backup` уже
   держит свежую точку вне хоста (`docker compose exec -T backup lidradar-backup status`), а
   локальная копия перед выпуском ускоряет откат на том же хосте:

   ```bash
   cd /opt/lidradar/deploy/production
   LIDRADAR_BACKUP_DIR=/var/backups/lidradar ../../scripts/backup.sh
   ```

   Запишите текущее значение `LIDRADAR_VERSION`.
3. Задайте новый `LIDRADAR_VERSION` в `.env` и проверьте `docker compose config --quiet`.
4. Остановите приложение, примените миграции, запустите новое:

   ```bash
   docker compose stop api worker scheduler
   docker compose run --rm migrate
   docker compose up -d
   ```

   Если `migrate` завершился с ошибкой, не запускайте приложение: смотрите вывод,
   затем «Откат».
5. Проверьте `docker compose ps` и приёмку из раздела 5 с `--version <новый тег>`.
6. Старые образы оставьте до следующего выпуска, остальные уберите:
   `docker image ls 'lidradar-*'`, `docker rmi …`.

Образ `postgres:18-alpine` привязан к мажорной версии. Его минор обновляют
осознанно, после копии: `docker compose pull postgres && docker compose up -d postgres`
(на время перезапуска базы приложение недоступно).

## 7. Откат выпуска

Откат одного образа безопасен, только если `migrate` ничего не применил. В
остальных случаях откат — это возврат базы к копии, снятой перед обновлением.
Проверено 2026-10-10 на стеке выше (9 секунд на копии в 250 КБ; время растёт с
размером базы):

```bash
cd /opt/lidradar/deploy/production
docker compose stop api worker scheduler
../../scripts/restore.sh /var/backups/lidradar/lidradar-<штамп>.dump lidradar_restored
../../scripts/bootstrap-roles.sh lidradar_restored
docker compose exec -T postgres psql -U lidradar -d postgres -v ON_ERROR_STOP=1 <<SQL
ALTER DATABASE lidradar RENAME TO lidradar_failed_$(date +%Y%m%d);
ALTER DATABASE lidradar_restored RENAME TO lidradar;
SQL
```

Верните прежний `LIDRADAR_VERSION` в `.env`, выполните `docker compose up -d` и
приёмку. Данные, записанные после копии, потеряны. Неудачная база `lidradar_failed_<дата>` остаётся для
разбора и удаляется отдельно (`DROP DATABASE`). `RENAME` отказывает при активных
соединениях с базой: приложение должно быть остановлено. Восстановление на новом
хосте и роли после него — [backup-restore.md](backup-restore.md).

## 8. Копии и ключ

Копии по расписанию и вне хоста снимает служба `backup`: каждые 10 минут выгрузка, шифрование
открытым ключом age, загрузка в S3-совместимое хранилище, сверка и манифест
([ADR 0051](../adr/0051-offhost-backups.md)). Подготовка хранилища и ключей, проверка,
восстановление на новом хосте и ротация — [offhost-backup.md](offhost-backup.md). Состояние:

```bash
cd /opt/lidradar/deploy/production
docker compose exec -T backup lidradar-backup status      # возраст последней подтверждённой точки
```

Ручная локальная копия перед обновлением и учение по локальной копии остаются, они работают
через `docker compose exec postgres` под владельцем `lidradar`, порт базы не нужен
(имена пользователя и базы в стеке фиксированы, прежние значения скриптов подходят):

```bash
LIDRADAR_BACKUP_DIR=/var/backups/lidradar ../../scripts/backup.sh        # копия с ротацией (по умолчанию 14)
LIDRADAR_BACKUP_DIR=/var/backups/lidradar ../../scripts/restore-drill.sh # копия → отдельная база → сверка → удаление
```

Сохраните копию `LIDRADAR_INTEGRATION_ENCRYPTION_KEY` и `.env` отдельно от копий базы (у операторов, не
на хосте): ключ приложения в копию базы не входит. Измеренные RPO и RTO и учение на новом
хосте с настоящим хранилищем — условия RG-DR; их результаты записывают в
[RG-DR](../engineering/RELEASE_GATES.md#rg-dr).

## 9. Повседневное

| Задача | Команда (из `deploy/production`) |
|---|---|
| Состояние и здоровье | `docker compose ps` |
| Журнал сервиса | `docker compose logs -f --tail 100 api` (JSON, события — [10-operations.md](../backend/10-operations.md) § 5) |
| Готовность изнутри | `docker compose exec -T api wget -qO- http://127.0.0.1:8080/health/ready` |
| Перезапуск сервиса | `docker compose restart api` |
| Возраст последней копии вне хоста | `docker compose exec -T backup lidradar-backup status` |
| Остановить всё, сохранив данные | `docker compose stop` |
| Занятое место | `docker system df` |

Журналы хранятся на хосте с ротацией 10 МБ × 5 файлов на сервис. **Не выполняйте
`docker compose down -v`**: флаг удаляет тома с базой и сертификатами. После
перезагрузки хоста контейнеры поднимаются сами (проверено: стек здоров через 7 с,
`api` и другие процессы, пришедшие раньше базы, перезапускаются политикой).

## 10. Диагностика

| Симптом | Вероятная причина | Что делать |
|---|---|---|
| `docker compose` печатает «задайте … в .env» | пропущено обязательное значение | заполнить переменную в `.env` |
| `Pool overlaps with other one` или `Address already in use` у `edge` | подсеть `172.29.88.0/24` занята сетью хоста | задать `LIDRADAR_EDGE_SUBNET`, `LIDRADAR_EDGE_IP`, `LIDRADAR_EDGE_DYNAMIC_RANGE` вместе (адрес edge вне динамического пула) |
| `edge` не получает сертификат | DNS не указывает на хост, порты `80`/`443` закрыты | `docker compose logs edge`, проверить DNS и брандмауэр снаружи |
| `502` на `/api/…`, страница открывается | `api` не готов или остановлен | `docker compose ps`, `docker compose logs api` |
| `403 ORIGIN_NOT_ALLOWED` при входе | адрес в браузере не совпадает с `https://<LIDRADAR_DOMAIN>` или `LIDRADAR_PUBLIC_ORIGIN` | выровнять адрес и настройку |
| у всех пользователей `429` | API не видит адрес клиента | журнал `http.forwarded_header_ignored`, раздел «Проверка адреса клиента», [proxy-deployment.md](proxy-deployment.md) |
| `503` на `/health/ready` после обновления | схема базы не соответствует сборке | выполнить `docker compose run --rm migrate` той же версии; иначе раздел 7 |
| `api` завершается с `runtime.configuration_invalid` | в журнале перечислены недостающие настройки | дополнить `.env` |
| `api` завершается с `connect to PostgreSQL` | база не готова либо пароль в `.env` не совпадает с паролем в базе (он задаётся при создании тома) | `docker compose ps postgres`; при «password authentication failed» выровнять пароль ([раздел 11](#11-роли-базы-данных)); иначе процессы подхватят базу сами после её старта |
| `role "lidradar_runtime" does not exist` | том создан по прежней схеме (суперпользователь), файл ролей при инициализации не выполнялся | перенести данные восстановлением копии в новый том, [раздел 11](#переход-со-схемы-с-суперпользователем) |
| `pg_hba.conf rejects connection … user "postgres"` | вход администратора по сети закрыт намеренно | администрировать по сокету: `docker compose exec postgres psql -U postgres -d lidradar` |
| `must be owner of table …` в журнале процесса | процесс выполняет DDL под рабочим логином | DDL выполняет только владелец (`migrate`); для разовой правки схемы — `docker compose exec postgres psql -U lidradar …` |
| поток событий рвётся | между клиентом и edge стоит буферизующий посредник | убрать буферизацию; в Caddy `flush_interval -1` уже задан |

## 11. Роли базы данных

[ADR 0052](../adr/0052-database-roles-owner-and-runtime.md). Встроенная база работает под тремя
учётными записями; при первой инициализации тома их создаёт
[`postgres/10-roles.sql`](../../deploy/production/postgres/10-roles.sql), а сетевой вход
ограничивает [`postgres/pg_hba.conf`](../../deploy/production/postgres/pg_hba.conf).

| Запись | Для чего | Пароль | Кто его знает |
|---|---|---|---|
| `postgres` | администратор кластера, только по сокету внутри контейнера | `POSTGRES_PASSWORD` | контейнер `postgres`; по сети вход закрыт |
| `lidradar` | владелец базы: миграции, восстановление копий; не суперпользователь | `LIDRADAR_DB_OWNER_PASSWORD` | `postgres` (инициализация), одноразовый `migrate` |
| `lidradar_runtime` | рабочий логин: api, worker, scheduler, копии, инструменты; не владелец | `LIDRADAR_DB_RUNTIME_PASSWORD` | `postgres`, api, worker, scheduler, backup, инструменты |

Администрирование выполняют по сокету, пароль не нужен (доступ к контейнеру равен доступу
администратора хоста): `docker compose exec postgres psql -U postgres -d lidradar`. Права
владельца достаточны для правки схемы вручную и восстановления (`-U lidradar`); рабочий логин
схему менять не может, и это намеренно: триггеры «только добавление» и RLS ему недоступны.

**Смена пароля** (проверено на живом стеке; пример для рабочего логина, владельца меняют так же):

```bash
cd /opt/lidradar/deploy/production
docker compose exec -T -e LIDRADAR_DB_OWNER_PASSWORD='<текущий пароль владельца>' -e LIDRADAR_DB_RUNTIME_PASSWORD='<новый пароль>' \
  postgres psql -v ON_ERROR_STOP=1 -U postgres -d lidradar -f /docker-entrypoint-initdb.d/10-roles.sql
# затем тот же пароль в .env и:
docker compose up -d
```

Файл идемпотентен: он возвращает атрибуты, членство и пароли к описанным. Прежний пароль
перестаёт приниматься сразу, процессы со старыми соединениями работают до пересоздания, а
`docker compose up -d` пересоздаёт `postgres` (его окружение входит в конфигурацию) и процессы
приложения: перерыв порядка 15 секунд. Пароль администратора в базе не меняют: тот же файл его не
трогает, администратор по сети не входит, а команда выше использует сокет.

**Приёмка** читает роли с живого стека (`scripts/smoke-production.sh … --compose-dir`):
записи без суперправ, база принадлежит владельцу, к ней не подключён суперпользователь,
процессы подключены рабочим логином.

### Переход со схемы с суперпользователем

Стек, запущенный по схеме ADR 0050 (приложение под суперпользователем `lidradar`), на месте не
переводится: суперпользователь, созданный образом, неизменяем. Данные переносят восстановлением
копии вне хоста в новый том, как при потере хоста
([offhost-backup.md](offhost-backup.md) § 4.1); проверено переносом копии, снятой под
суперпользователем, на новую схему:

1. Убедитесь, что есть свежая копия вне хоста, и сделайте учение `drill` ([offhost-backup.md](offhost-backup.md) § 2).
2. Обновите файлы выпуска (`deploy/production/` с каталогом `postgres/`) и `.env`: три пароля вместо
   одного (`POSTGRES_PASSWORD` меняет смысл: теперь это администратор), `POSTGRES_USER` и `POSTGRES_DB`
   больше не используются.
3. `docker compose down` и удаление тома базы (`docker volume rm <проект>_lidradar-postgres`) после
   проверки копии; `docker compose up -d --wait postgres` (файл ролей выполнится при инициализации,
   команда вернётся, когда база начнёт принимать подключения по сети).
4. `../../scripts/restore-offhost.sh --identity … --replace-database`, затем `docker compose up -d` и приёмка.

Данные после последней копии потеряны, поэтому перед шагом 3 остановите запись (`docker compose stop api
worker scheduler`) и дождитесь свежей копии.

### Внешняя управляемая база

Не проверялось на настоящем поставщике. Администратор базы выполняет файл ролей в базе приложения
(нужен `psql` 15 или новее; пароли из окружения):

```bash
LIDRADAR_DB_OWNER_PASSWORD=… LIDRADAR_DB_RUNTIME_PASSWORD=… psql -d БАЗА -f deploy/production/postgres/10-roles.sql
```

Затем в `.env` задают `LIDRADAR_DATABASE_URL` (рабочий логин, `sslmode=verify-full`),
`LIDRADAR_MIGRATE_DATABASE_URL` (владелец), оставляют `LIDRADAR_DATABASE_ALLOW_PLAINTEXT=false` и
отключают встроенный `postgres` override-файлом. Compose при этом всё равно требует значения трёх
паролей из § 2 (любые непустые): вложенная проверка вычисляется и при заданных адресах.

## Что не закрыто

- Не проверены на настоящем хосте: выпуск сертификата, Linux (проверка шла на Docker
  Desktop), поведение за облачным брандмауэром и балансировщиком, адрес клиента по
  IPv4 и IPv6, нагрузка стека целиком, обновление с миграцией схемы, внешняя база
  через `LIDRADAR_DATABASE_URL` (`sslmode=verify-full`, сервис `postgres` убирают
  своим override-файлом).
- Копии по расписанию и вне хоста есть (ADR 0051), но не проверены на настоящем поставщике
  хранилища и Linux-хосте; нет измеренных на реальных данных RPO и RTO и PITR. Хранение
  `.env` и ключей у операторов ручное.
- Владелец встроенной базы `lidradar` не суперпользователь, но остаётся мощной записью: он может по
  имени отключить пользовательский триггер или политику RLS. Его пароль есть только у одноразового
  `migrate`; утёкший пароль меняют по § 11. Рабочий логин остаётся членом `lidradar_platform`
  (границы ADR 0041).
- Секреты хранятся открытым текстом в `.env` (права `600`) и видны членам группы
  `docker`. Ротация ключа шифрования, реестр образов, автоматическая доставка,
  манифест выпуска, мониторинг и вынос журналов за пределы хоста отсутствуют.
- Хост — единая точка отказа. Регистрация пользователей открыта.
