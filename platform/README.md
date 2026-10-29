# Platform — backend 1

Сервис управления испытаниями Полигона. Все исходники, контракты, миграции, тесты и конфигурация этого сервиса находятся внутри `platform/`. Остальные компоненты используются через внешние интерфейсы.

Реализованы HTTP API, BFF-авторизация, права проектов, immutable версии и планы, credentials, создание и отмена запусков, leases, outbox/inbox, supervisor, отдельные серии оценки, артефакты, SSE, отчёты и сравнение. Go + net/http, PostgreSQL/pgx, grpc-go, NATS JetStream, S3 SDK, OpenTelemetry.

Сквозная работа всего Полигона требует Runner, Environment и Intelligence. В исходном репозитории у них пока только README. Тестовые providers используются исключительно в тестах; приложение не подменяет недоступные сервисы успешными ответами. Wire-контракт validation RPC пока является предложением для согласования с их владельцами. По умолчанию временный адаптер выключен; для согласованного теста предложения задать `VALIDATOR_WIRE_MODE=struct-proposal`. После получения их `.proto` нужен сгенерированный provider client за тем же интерфейсом.

## Локальный запуск

Нужны Docker Engine и Docker Compose.

```sh
cd platform
python3 scripts/local_env.py
docker compose up --build -d
```

Скрипт создаёт `.env` с новыми случайными секретами и правами `0600`; существующий файл не перезаписывает. В Compose запускаются только инфраструктура Platform и сам сервис: PostgreSQL, NATS, MinIO, создание bucket, миграции, каталог и API.

- HTTP: `http://localhost:8080`.
- Control gRPC: `localhost:9090`.
- Health: `/livez`, `/readyz`.
- Вход local-пользователя: `/api/v1/auth/login`; профиль и CSRF: `/api/v1/me`.
- MinIO: API `localhost:9000`, console `localhost:9001`.
- Environment и Intelligence по умолчанию ожидаются на хосте: `9091` и `9092`.

Без validators можно войти и работать с проектами, агентами и drafts. Публикация сценария и запуск вернут 503 до подключения зависимостей. Frontend в этот образ не входит; `/` не является веб-интерфейсом.

## Запуск Go на хосте

```sh
python3 scripts/local_env.py # только если .env ещё нет
set -a
. ./.env
set +a
docker compose up -d postgres nats minio bucket
go run ./cmd/platform migrate
go run ./cmd/platform seed
go run ./cmd/platform serve
```

Зависимости зафиксированы в `go.mod`/`go.sum`; версия Go указана в `go.mod`. Миграции применяются отдельной командой под advisory lock. Сервис не меняет схему автоматически при старте.

## Контракты

- `contracts/openapi/platform.v1.json`: OpenAPI 3.1, 42 пути и 56 операций.
- `contracts/openapi/client.ts`: сгенерированные TypeScript-типы и клиент JSON API.
- `contracts/proto/poligon/control/v1/control.proto`: восемь Control RPC.
- `gen/controlv1`: сгенерированный Go transport.
- `contracts/schemas`: схемы агента, сценария, ArtifactRef, manifest и assessment.
- `contracts/catalog`: начальные operator-owned определения, которые должны поддержать providers.
- `contracts/fixtures`: проверяемые transport fixtures; это не результат запуска настоящего агента.

`python3 scripts/contracts.py` обновляет OpenAPI и TypeScript. `make generate` дополнительно требует `protoc`, `protoc-gen-go` и `protoc-gen-go-grpc`. Для входа, скачивания и SSE использовать browser navigation/EventSource: универсальный JSON-клиент не обрабатывает эти потоки.

План передаётся в `runtime_plan_json` как JSON bytes. Hash вычисляется по JCS/RFC 8785, а не по форматированию JSON из PostgreSQL. `evaluation_definition_json` и её hash содержат назначенный evaluator для manual/retry jobs; исходный план при переоценке не меняется.

Интеграция и форматы сообщений: [docs/integration.md](docs/integration.md).

## Авторизация

Local auth включается только явным `APP_ENV=local` при отсутствии `OIDC_ISSUER`. Для OIDC задать issuer, client ID и при необходимости client secret. Сервер реализует code flow с PKCE и проверяет state, nonce, issuer, audience, подпись и сроки токена.

```sh
docker compose --profile oidc up -d keycloak
```

Для Go-процесса на хосте: `OIDC_ISSUER=http://localhost:8081/realms/poligon`, `OIDC_CLIENT_ID=poligon-platform`, пустой `OIDC_CLIENT_SECRET`. Импортирован local-only пользователь `developer` с паролем `local-development-only`. Для контейнерного OIDC issuer должен разрешаться одинаково из браузера и контейнера; настроить общий DNS/reverse proxy. Профиль Compose предназначен для локальной разработки.

Cookie-сессия непрозрачная, срок 8 часов; в БД только hash. Cookie HttpOnly/SameSite=Lax, production — Secure. Изменяющие запросы требуют `Origin`, совпадающий с `PUBLIC_ORIGIN`, и `X-CSRF-Token` из `/me`. Logout отзывает серверную сессию и закрывает SSE при следующей проверке доступа. Email не используется как ключ пользователя.

Роли: owner управляет участниками и credentials, editor редактирует версии и запускает испытания, viewer читает. Последнего owner удалить или понизить нельзя. Чужие ресурсы отвечают 404.

## Проверки

```sh
make lint
make test
make contracts
make build

# Выделенная тестовая БД: пользователь должен иметь CREATEDB.
TEST_DATABASE_URL='postgres://platform:password@localhost:5432/postgres?sslmode=disable' \
TEST_NATS_URL='nats://localhost:4222' make integration
```

Каждый DB-тест создаёт собственную временную database и удаляет её после завершения. NATS-проверке нужен отдельный тестовый JetStream без конкурирующего Runner. Не указывать рабочие инфраструктурные credentials. Без переменных DB/NATS соответствующие интеграционные тесты явно пропускаются.

Покрыты конкурентная HTTP-идемпотентность, CSRF/tenant isolation/last owner, OIDC с подписанными токенами, If-Match, недоступный validator, повторный claim, stale attempt, lease recovery, отмена, manual evaluation, immutable upload/checksum, executed→assessment→report, восстановление progress, SSE replay/logout и 20 активных execution leases.

Текущие результаты и ограничения проверки: [docs/verification.md](docs/verification.md).

## Эксплуатация

- Настройки передаются через env, пример — `.env.example`.
- Concurrency: 5 на проект по умолчанию, 50 глобально; оценки — 5/20 отдельно.
- Оператор меняет проектную квоту: `platform quota PROJECT_UUID 20`.
- Глобальные лимиты: `GLOBAL_CONCURRENCY`, `GLOBAL_EVALUATION_CONCURRENCY`.
- Логи JSON; OTLP export включается через `OTEL_EXPORTER_OTLP_ENDPOINT`.
- `/metrics` требует `Authorization: Bearer $METRICS_TOKEN`.
- NATS ACL в `deploy/nats.conf`; bounded streams используют `DiscardNew`.
- Credentials зашифрованы AES-256-GCM; `CREDENTIAL_MASTER_KEY` хранить отдельно от БД.
- Retention артефактов — минимум 30 дней. Удаление запускается оператором: `platform purge-expired --apply`. Активный run или evaluation не удаляется; неудачное удаление повторяется по durable reservation.
- Проверка хранилища после restore: `platform verify-artifacts`.

Runbook и restore: [docs/operations.md](docs/operations.md). Решения и ограничения: [docs/decisions.md](docs/decisions.md).
