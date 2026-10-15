# Первый выпуск: внутренняя организация на хосте Beget

Редакция 2026-10-15. Памятка «в день выпуска» для первой ступени пилота (этап 26,
LR-BE-2610): стек на новом Linux-хосте, одна **внутренняя** организация, домашний AI-узел,
живая проверка Telegram. Клиентов приглашают только после раздела 10. Подробности и
диагностика — в [production-deployment.md](production-deployment.md); копии —
[offhost-backup.md](offhost-backup.md). Памятка проверена на Docker Desktop (Mac, arm64)
репетицией из чистого клона коммита выпуска: сборка образов под `linux/amd64`, `up -d --wait`
за 14 секунд, `smoke-production.sh` без замечаний, сквозной сценарий от приглашения до
подтверждённой выручки. На настоящем Linux-хосте, у настоящего поставщика хранилища и по
Telegram-матрице она **не** проходилась: это и есть содержание дня.

```bash
BACKEND=~/Develop/Lidradar               # репозиторий бэкенда (каталоги deploy/, scripts/)
WEB=~/WebstormProjects/Lidradar          # репозиторий веб-клиента
DOMAIN=app.example.ru                    # ваш домен
HOST=deploy@203.0.113.5                  # пользователь и адрес сервера
```

## 1. Что подготовить заранее (самое долгое — сначала)

| Что | Зачем | Заметки |
|---|---|---|
| Сервер Linux x86_64 (Ubuntu 24.04 или 22.04), от 2 vCPU, 4 ГБ, 30 ГБ SSD | стек целиком | Доступ по SSH-ключу. В панели хостера откройте входящие `22`, `80`, `443`; остальное закрыто. |
| Домен и A-запись на адрес сервера | сертификат выпускает Caddy при первом запуске | **AAAA-запись не создавайте**: адрес клиента по IPv6 за портами Docker не проверен. Проверка: `dig +short $DOMAIN`. |
| Хранилище копий (S3-совместимое) и ключи | **без них стек не запустится** | Лучше поставщик, отличный от хостера сервера, и в РФ (копии содержат переписки клиентов, решение о месте хранения — ваше, ADR 0056 п. 7). Нужны бакет, версионирование, ключ записи, ключ чтения ([offhost-backup.md](offhost-backup.md) § 1). Если поставщик не умеет ключ «только запись», подойдёт ключ на этот бакет с чтением и записью: слабее, поэтому версионирование обязательно. |
| Пара ключей `age` минимум у двух хранителей | расшифровка копий | `age-keygen` на вашем компьютере; **приватные ключи на сервер не кладут**; потеря всех приватных ключей делает копии нечитаемыми. |
| Почта для писем сброса пароля | без `LIDRADAR_MAIL_*` не запустится `worker` | См. п. 1.1 про доступность SMTP. |
| Боевой бот Telegram | уведомления и привязка `/start` | BotFather: токен и имя без `@`. Для проверки каналов нужны **два** живых аккаунта Telegram. |
| Домашний узел | семантические риски R2–R5 | GPU-хост с моделью; статичный адрес у вас есть. Без узла работает риск «нет ответа», остальные ждут. |
| Место для секретов | `.env`, ключ шифрования, приватные ключи age | Менеджер паролей или зашифрованный носитель у двух людей, не на сервере. |

### 1.1. Доступность внешних сервисов с российского хоста

Репетиция показала два предупреждения. **Docker Hub** из домашней сети отвечал с таймаутами TLS (образы
скачивались со 2–5 попытки). **SMTP Gmail** с этого же Mac закрывал соединение (`SMTP_NETWORK: EOF`),
поэтому не рассчитывайте на `smtp.gmail.com` заранее: запустите проверку хоста (раздел 3) сразу после
создания сервера, она проверит и SMTP, и Docker Hub, и Telegram, и хранилище. Если Gmail с сервера
недоступен, возьмите SMTP российского сервиса или почтовый ящик на своём домене (адреса получателей тогда
не уходят Google, что и для юриста проще): в `.env` задайте `LIDRADAR_MAIL_SMTP_HOST`, `_PORT`,
`_SECURITY` (`starttls` для 587, `tls` для 465), `LIDRADAR_MAIL_SMTP_USERNAME`, `_PASSWORD`,
`LIDRADAR_MAIL_FROM`. Пока SMTP не заработал, `worker` запускается, но письма сброса пароля и
подтверждения не уходят (`mail.smtp.check_failed` в его журнале); вход по коду приглашения от почты не
зависит.

## 2. Сборка и доставка образов (на вашем Mac)

Образы собираются **только из коммита**, дерево обоих репозиториев должно быть чистым
(`git status --short` пусто). Mac на arm64, сервер на x86_64, поэтому платформа задаётся явно;
сторонние образы `caddy` и `postgres` едут тем же архивом, чтобы сервер не зависел от Docker Hub.

```bash
cd "$BACKEND" && git status --short && git -C "$WEB" status --short   # оба пусты
DOCKER_DEFAULT_PLATFORM=linux/amd64 scripts/build-images.sh --web-dir "$WEB"
# последняя строка называет LIDRADAR_VERSION=<тег>; тег — первые 12 символов коммита бэкенда
TAG=$(git rev-parse --short=12 HEAD)
docker pull --platform linux/amd64 caddy:2.11.7-alpine
docker pull --platform linux/amd64 postgres:18-alpine
docker save --platform linux/amd64 \
  $(for c in api worker scheduler migrate backup platform-admin ai-node-register ai-node-manage web; do echo lidradar-$c:$TAG; done) \
  caddy:2.11.7-alpine postgres:18-alpine | gzip > /tmp/lidradar-$TAG.tar.gz
ls -lh /tmp/lidradar-$TAG.tar.gz                       # репетиция: 241 МБ, сохранение 6 секунд
# Docker с хранилищем containerd после pull/load привязывает локальные теги к amd64 (дальше всё на Mac
# пойдёт под эмуляцией): верните arm64-варианты для локальной работы.
docker pull --platform linux/arm64 caddy:2.11.7-alpine postgres:18-alpine
```

Сборка под эмуляцией amd64 занимает несколько минут (репетиция: все десять образов, включая веб-клиент,
249 с при тёплых базовых образах; образ `api` на холодную — 112 с вместе с их загрузкой). Доставка:

```bash
scp /tmp/lidradar-$TAG.tar.gz $HOST:/tmp/
git archive HEAD deploy scripts | ssh $HOST 'mkdir -p /opt/lidradar && tar -x -C /opt/lidradar'
ssh $HOST "gunzip -c /tmp/lidradar-$TAG.tar.gz | docker load && rm /tmp/lidradar-$TAG.tar.gz"
```

Файлы `deploy/` и `scripts/` на хосте должны быть того же коммита, что и образы. Образ AI-агента для
домашнего узла (`lidradar-ai-agent:<тег>`, тот же `build-images.sh`) отправляется отдельно, раздел 7.

## 3. Подготовка сервера (один раз)

Под `root` или через `sudo`, на Ubuntu:

```bash
apt-get update && apt-get install -y ca-certificates curl gnupg chrony jq python3
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" > /etc/apt/sources.list.d/docker.list
apt-get update && apt-get install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
adduser --disabled-password --gecos "" deploy && usermod -aG docker deploy
install -d -o deploy -g deploy /opt/lidradar
```

Если `download.docker.com` с хоста недоступен, запасной путь: пакеты `docker.io` и `docker-compose-v2` из
репозитория Ubuntu (проверьте `docker compose version`, нужна версия 2). Затем скопируйте ключ SSH
пользователю `deploy` (`/home/deploy/.ssh/authorized_keys`, права `700`/`600`), проверьте вход вторым
окном и только после этого запретите вход по паролю (`PasswordAuthentication no` в
`/etc/ssh/sshd_config`, `systemctl restart ssh`). Пользователь группы `docker` фактически root: в неё входят
только операторы.

Проверка хоста перед первым запуском (ничего не меняет, называет всё, что мешает):

```bash
cd /opt/lidradar
scripts/preflight-host.sh --domain $DOMAIN --smtp smtp.gmail.com:587 \
  --s3-endpoint https://<адрес хранилища> --compose-dir deploy/production
```

Каждая строка — `ok`, `warn` или `FAIL`; код `1` означает блокирующее замечание. Разберите все `FAIL`:
закрытый исходящий SMTP, недоступное хранилище или Let's Encrypt, занятые порты `80`/`443`, DNS не на этот
хост. `warn` про Docker Hub безвреден, если образы приехали архивом.

## 4. Окружение

```bash
cd /opt/lidradar/deploy/production
cp .env.example .env && chmod 600 .env
openssl rand -hex 24      # POSTGRES_PASSWORD
openssl rand -hex 24      # LIDRADAR_DB_OWNER_PASSWORD
openssl rand -hex 24      # LIDRADAR_DB_RUNTIME_PASSWORD
openssl rand -base64 32   # LIDRADAR_INTEGRATION_ENCRYPTION_KEY
```

Значения вносите редактором (не в командной строке: она попадёт в историю). Обязательные:

| Переменная | Значение |
|---|---|
| `LIDRADAR_VERSION` | тег из раздела 2 |
| `LIDRADAR_DOMAIN`, `LIDRADAR_ACME_EMAIL` | домен и ваша почта для уведомлений центра сертификации |
| `POSTGRES_PASSWORD`, `LIDRADAR_DB_OWNER_PASSWORD`, `LIDRADAR_DB_RUNTIME_PASSWORD` | три **разных** пароля |
| `LIDRADAR_DATABASE_ALLOW_PLAINTEXT` | `true` для встроенной базы (решение оператора, ADR 0050) |
| `LIDRADAR_INTEGRATION_ENCRYPTION_KEY` | вывод `openssl rand -base64 32`; **сразу сохраните копию вне сервера** |
| `LIDRADAR_TELEGRAM_TOKEN`, `LIDRADAR_TELEGRAM_BOT_USERNAME` | токен и имя боевого бота |
| `LIDRADAR_MAIL_SMTP_USERNAME`, `_PASSWORD`, `LIDRADAR_MAIL_FROM` (и при другом сервисе `_HOST`, `_PORT`, `_SECURITY`) | раздел 1.1 |
| `LIDRADAR_BACKUP_REMOTE` | `offhost:<бакет>/<префикс>` |
| `LIDRADAR_BACKUP_S3_ENDPOINT`, `_ACCESS_KEY_ID`, `_SECRET_ACCESS_KEY` (и `_REGION`, если требует поставщик) | данные ключа записи |
| `LIDRADAR_BACKUP_AGE_RECIPIENTS` | публичные ключи `age1…` через запятую |
| `LIDRADAR_BACKUP_PING_URL`, `LIDRADAR_BACKUP_FAIL_URL` | необязательно: адреса сервиса «пульса» (тревога, если копия не пришла) |

```bash
docker compose config --quiet && echo 'конфигурация корректна'
```

## 5. Запуск и приёмка

```bash
docker compose up -d --wait
docker compose ps
```

Ожидание (репетиция): `migrate` завершён с кодом 0, `postgres`, `api`, `web`, `edge`, `backup` — `healthy`,
`worker` и `scheduler` — `running`, всё за 15–30 секунд, `backup` снимает первую точку сразу. Сертификат:

```bash
docker compose logs edge | grep 'certificate obtained'
```

Если сертификата нет через несколько минут, проверьте DNS и порты снаружи (раздел 10 общей памятки
[production-deployment.md](production-deployment.md#10-диагностика)); у Let's Encrypt лимит неудач в час.

Приёмка. Снаружи (с вашего Mac, он проверяет и закрытые порты):

```bash
cd "$BACKEND" && scripts/smoke-production.sh https://$DOMAIN
```

На хосте (с версией, миграциями, копией и ролями базы):

```bash
cd /opt/lidradar && scripts/smoke-production.sh https://$DOMAIN --closed-ports '' \
  --version <тег> --compose-dir deploy/production
```

Оба запуска должны закончиться «замечаний нет». Затем два обязательных ручных пункта:

1. **Адрес клиента.** Откройте `https://$DOMAIN` с вашего компьютера и войдите (после п. 6). В
   `sessions.ip` должен стоять ваш публичный IPv4, а не `172.29.88.1`/`172.29.88.2`:

   ```bash
   docker compose exec -T postgres psql -U lidradar -d lidradar -At \
     -c 'select ip, created_at from sessions order by created_at desc limit 3'
   ```

   Подробности и что делать при неверном адресе — [production-deployment.md](production-deployment.md#проверка-адреса-клиента-обязательна-на-настоящем-хосте).
2. **Почта.** `docker compose logs worker | grep mail.smtp`: нужно `mail.smtp.ready`. Затем «Забыли пароль?» на
   странице входа для своего адреса: письмо должно прийти за минуту.

## 6. Первый администратор и внутренняя организация

```bash
docker compose run --rm platform-admin invite --note "владелец" --days 3     # код показывается один раз
```

Откройте `https://$DOMAIN/register?code=<код>`, зарегистрируйтесь, создайте организацию (мастер: компания,
точка, часы работы, услуги с ценами). Затем право платформенного администратора:

```bash
docker compose run --rm platform-admin grant --email <ваша почта> --note "первый администратор"
docker compose run --rm platform-admin list
```

Проверьте, что регистрация без кода закрыта: `curl -s https://$DOMAIN/api/v1/auth/registration` отвечает
`{"mode":"invite_only"}`. Для быстрой проверки всей цепочки без Telegram используйте вебхук-канал
(`GENERIC_WEBHOOK`, «Интеграции»): отправьте через `curl` входящее сообщение с `sentAt` на час раньше
текущего времени и словом из названия услуги — через несколько секунд воркер и планировщик откроют риск
«нет ответа» (репетиция: так проходит вся цепочка до подтверждённой выручки).

## 7. Домашний AI-узел

Организация уже есть, поэтому узел можно регистрировать (регистрации нужен идентификатор организации из
`/auth/me` или раздела «Администрирование → Организации»). На сервере:

```bash
cd /opt/lidradar/deploy/production
install -d -m 700 secrets
docker compose run --rm --user "$(id -u):$(id -g)" -v "$PWD/secrets:/out" \
  ai-node-register --tenant-id <uuid организации> --name home-gpu --output /out/ai-node.json
```

Файл `secrets/ai-node.json` (права `0600`) передайте на домашний узел по защищённому каналу
(`scp`), на сервере удалите. Образ агента на домашний узел собирается тем же `build-images.sh` (тег
`lidradar-ai-agent:<тег>`); узел на x86_64, поэтому платформа `linux/amd64`:

```bash
docker save --platform linux/amd64 lidradar-ai-agent:$TAG | gzip | ssh <домашний узел> 'gunzip | docker load'
git archive HEAD docker-compose.ai.yml | ssh <домашний узел> 'mkdir -p /opt/lidradar-ai && tar -x -C /opt/lidradar-ai'
```

На домашнем узле, рядом с `docker-compose.ai.yml`, файл `.env`:

```
LIDRADAR_AI_CLOUD_URL=https://<домен>
LIDRADAR_AI_CREDENTIALS_HOST_FILE=/srv/lidradar/secrets/ai-node.json
LIDRADAR_AI_AGENT_IMAGE=lidradar-ai-agent:<тег>
LIDRADAR_AI_MODELS_DIR=/srv/lidradar/models
LIDRADAR_AI_MODEL_FILE=Qwen3-8B-Q4_K_M.gguf
```

Запуск: `docker compose -f docker-compose.ai.yml up -d`. Узел сам ходит к API (опрос заданий и сердцебиение),
входящих портов дома не нужно. Часы узла должны быть синхронизированы (`chrony` или `systemd-timesyncd`):
подпись каждого запроса действует 60 секунд. Репетиция на боевом compose (агент с `fake`-провайдером):
узел регистрируется командой выше, становится `READY` за несколько секунд, берёт задание после нового
сообщения и завершает его (`ai_jobs` — `SUCCEEDED`, `ai_runs` — одна запись). Проверка: `docker compose -f docker-compose.ai.yml logs ai-agent` без
повторяющихся ошибок и «Администрирование → AI» показывает узел `READY`; через минуту после нового
сообщения в переписке появляется семантический факт. Версия модели узла и Cloud Core должна совпадать
(`LIDRADAR_AI_MODEL_VERSION`, по умолчанию `lidradar-main-v1`), иначе узел получает пустые задания.

После того как узел заработал, **закройте путь AI-узла на краю статичным адресом дома** (ADR 0050, уточнение
2026-10-15) и только после проверки адреса клиента (раздел 5): в `.env` сервера
`LIDRADAR_AI_NODE_ALLOWED_IPS="<статичный адрес>"`, затем `docker compose up -d edge`. С любого другого
адреса `curl -i -X POST https://$DOMAIN/internal/v1/ai/nodes/heartbeat` отвечает `403`, с адреса узла — `401`.
Если домашний адрес сменится, задания будут ждать в очереди (`PENDING`) до правки значения.

## 8. Живая проверка Telegram (матрица LR-BE-TG-004…010)

Это главный непройденный пункт ([TELEGRAM_SPIKE_REPORT.md](../roadmap/TELEGRAM_SPIKE_REPORT.md)): до сих пор
проверены только создание бота и доставка уведомления владельцу. Нужны аккаунт A («бизнес», подключён к
боту через Telegram Business) и аккаунт B («клиент»). Порог ответа точки на время проверки поставьте 1–2
минуты, часы работы круглосуточные.

1. «Интеграции» → подключить Telegram: токен бота вставляется один раз, после подключения проверка связи
   даёт `ACTIVE` (`webhook` установлен на `https://$DOMAIN/api/v1/webhooks/...`).
2. B пишет A: переписка появляется в LidRadar за секунды, сообщение `INCOMING` (TG-004).
3. A отвечает руками из Telegram: сообщение `OUTGOING` (TG-005); риск по этой переписке закрывается.
4. B правит сообщение, удаляет сообщение, присылает фото, голос и документ: правка меняет текст, удаление
   отмечает сообщение, вложения видны метаданными (TG-006).
5. B пишет снова, A молчит дольше порога: открывается риск «нет ответа» и уходит уведомление (TG-011).
6. Привязка `/start`: «Уведомления» → получить ссылку → открыть в Telegram → привязка видна в профиле (TG-010).
7. Отключить и снова подключить бота: состояние подключения корректно (TG-007).
8. Идентификаторы подключения, переписки, контакта и сообщения одинаковы после п. 7 (TG-009).

Результат по каждой строке — `проверено`/`не проверено` с отпечатком идентификаторов, без токенов и текстов —
в таблицу отчёта; повторы одного `update_id` (TG-008) покрыты тестами PostgreSQL, вживую их не воспроизвести.

## 9. Копии

```bash
docker compose exec -T backup lidradar-backup status          # возраст последней точки, подряд неудач
docker compose run --rm backup check                          # пробный цикл в _check/
```

Нужны: возраст точки меньше 15 минут, `подряд неудач: 0`. До приглашения клиентов проведите **учение по
восстановлению** ключом чтения и приватным ключом age: на вашем компьютере или втором хосте
([offhost-backup.md](offhost-backup.md) § 3 и 4: `lidradar-backup drill`). Запишите момент данных и время в
[RG-DR](../engineering/RELEASE_GATES.md#rg-dr). Убедитесь, что приватные ключи age у двух людей, а `.env` и
ключ шифрования лежат вне сервера.

## 10. Условия для первого клиента (go/no-go)

Приглашайте дружественного клиента после **суток** спокойной работы, когда выполнено всё:

1. Приёмка раздела 5 без замечаний; адрес клиента верный; сертификат выпущен.
2. `mail.smtp.ready` (или решение запускать без писем).
3. Копия свежая, учение по восстановлению проведено, ключи и `.env` сохранены вне сервера.
4. Матрица Telegram раздела 8 пройдена минимум по TG-004, TG-005, TG-010, TG-011.
5. Если подключён AI-узел: узел `READY`, один семантический факт получен; путь узла закрыт по адресу.
6. В журнале `worker` и `api` за последние 30 минут нет повторяющихся ошибок:
   `docker compose logs --since 30m worker api | grep '"level":"ERROR"'`.
7. Клиенту отправлен текст согласия и политики (юрист не привлекался; очистка по срокам хранения ещё не
   исполняется, это нужно сказать честно, ADR 0056), код приглашения выдан командой `invite` на одного человека.

## 11. Если что-то пошло не так

| Что случилось | Что делать |
|---|---|
| Стек не поднялся | `docker compose ps -a`, `docker compose logs <сервис>`; отсутствующее значение `docker compose config` называет по имени |
| Нет сертификата | DNS, порты снаружи, `docker compose logs edge`; том `caddy-data` не удалять |
| Письма не уходят | `docker compose logs worker \| grep mail.`; сменить SMTP в `.env` и `docker compose up -d worker` |
| Неверный адрес клиента | не пускать пользователей; раздел «Проверка адреса клиента» production-deployment.md |
| Нужно остановить | `docker compose stop` (данные сохраняются). **Не выполнять `docker compose down -v`**: удаляет базу и сертификаты |
| Испорчены данные или потерян хост | восстановление из копии вне хоста: [offhost-backup.md](offhost-backup.md) § 4; перед запуском всего стека поднимается только `postgres`, после восстановления повторяются стирания ([data-requests.md](data-requests.md) § 5) |
| Откат выпуска | для первого выпуска откатывать нечего; последующие обновления — [production-deployment.md](production-deployment.md#6-обновление-выпуска) |
