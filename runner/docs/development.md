# Разработка и эксплуатация Runner

## Локальное окружение

Для Go-команд нужен toolchain версии, указанной в [go.mod](../go.mod). Выполняйте команды из каталога `runner`.

1. Поднимите [закреплённую версию Platform](integration.md#версии) по её README. Platform создаёт потоки JetStream. Для подключения Runner используйте сервисную учётную запись с правами из `platform/deploy/nats.conf` того же checkout.
2. Запустите Environment командой `go run ./cmd/server` из каталога `environment` его закреплённого checkout.
3. Сверьте адреса из [шаблона](../.env.example) с локальным стендом. Если используется сервисный пользователь NATS, передайте оба поля учётных данных. При локальном Control без mTLS задайте токен Runner из настроек Platform. Шаблон нужно загрузить в окружение средствами оболочки или контейнера. Runner сам файл не читает.

Пример для PowerShell, когда `RUNNER_NATS_PASSWORD` и `RUNNER_CONTROL_TOKEN` уже заданы в окружении, а стенд использует адреса по умолчанию:

```powershell
$env:RUNNER_MODE = "local"
$env:RUNNER_NATS_USER = "runner"
go build ./...
go run ./cmd/runner
```

## Docker

[Dockerfile](../Dockerfile) собирает статический бинарник и запускает его от пользователя `runner`. Подготовьте локальный `.env` из [.env.example](../.env.example). Для примера ниже нужна существующая Docker-сеть с зависимостями. Укажите её имя вместо `poligon-local`, а в `.env` задайте доступные из контейнера адреса NATS, Control, Environment и хранилища. Адреса на loopback внутри контейнера относятся к самому Runner. Уберите строку `RUNNER_TEMP_DIR` из `.env`, чтобы сохранить значение образа, или укажите `/var/lib/runner` явно.

```sh
docker build -t poligon-runner:local .
docker run --rm --name poligon-runner --network poligon-local --env-file .env --stop-timeout 30 poligon-runner:local
```

Health check образа обращается к `http://127.0.0.1:8091/livez` внутри контейнера. При смене порта нужно согласовать команду проверки. При использовании mTLS передайте сертификаты и ключ как файлы, доступные пользователю Runner внутри контейнера.

## Тестирование

```sh
go test ./...
go test -race ./...
go vet ./...
```

Обычный набор тестов запускает JetStream и локальные gRPC, HTTP и WebSocket fixtures. На Windows для race detector нужен CGO и совместимый MinGW-w64. Укажите `CGO_ENABLED=1` и компилятор в `CC`. В [отчёте о проверках](verification.md) приведены команды для fuzz, нагрузочного стенда, настоящих Platform, Environment и MinIO. Команды `make build`, `make test`, `make race`, `make vet` и `make bench` определены в [Makefile](../Makefile).

## Диагностика и остановка

Runner пишет структурированные JSON-логи в stderr. Для диагностики используйте HTTP-адрес из `RUNNER_DIAGNOSTICS_ADDR`.

| Маршрут | Что проверить |
|---|---|
| `GET /livez` | HTTP 200, если процесс отвечает |
| `GET /readyz` | HTTP 503 при недоступной инфраструктуре или симуляторе Intelligence. Доступность NATS и Control сама по себе не означает готовность к диалогу |
| `GET /metrics` | Счётчики и длительности исполнения. На loopback токен можно не задавать. С заданным токеном нужен заголовок `Authorization: Bearer <token>`. Вне loopback без токена маршрут отсутствует |
| `GET /debug/pprof/` | Профили при `RUNNER_PPROF=true`, loopback-адресе и диагностическом токене |

При ошибке подключения проверьте адреса из окружения процесса, права пользователя NATS и настройки общего durable. При отказе загрузки проверьте доступность подписанного URL из сети Runner и соответствие его хоста `RUNNER_UPLOAD_HOSTS`. Для журналов выделите временный каталог с достаточной квотой. После аварийной остановки в нём могут остаться файлы.

Остановите локальный процесс через Ctrl+C или отправьте `SIGTERM`. Runner прекращает приём заданий и ждёт активные слоты до `RUNNER_DRAIN_TIMEOUT`. Дайте процессу время на drain и завершение служебных операций перед принудительной остановкой.
