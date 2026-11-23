# Environment на локальном стенде

## Запуск

Из каталога `environment/` скопируйте `.env.integration.example` в `.env.integration` и задайте три разных сервисных токена. Пары переменных указаны в шаблоне. Токен Runner хранится в его транспорте CRM и недоступен испытываемому агенту.

```sh
set -a
. ./.env.integration
set +a
go run ./cmd/server
```

HTTP слушает `127.0.0.1:8081`, gRPC — `127.0.0.1:9091`. Адрес Control по умолчанию — `127.0.0.1:9090`. Профиль соответствует приватному однохостовому стенду Platform с `APP_ENV=local`. Для загрузки Environment нужен сетевой доступ к адресу MinIO, который Platform включает в подписанный URL. Сервис использует собственный токен Control и только подписанный PUT в хранилище.

По умолчанию `go run ./cmd/server` без интеграционного окружения сохраняет автономный HTTP-режим. В нём действуют прежние `-port` и `PORT`, повторный `POST /api/v1/sessions` заменяет данные, а reset восстанавливает исходные фикстуры.

## Открытие и HTTP-вызовы Runner

`POST /api/v1/attempts` принимает `Authorization: Bearer <RUNNER_ENVIRONMENT_TOKEN>` и JSON из [open-request.json](../contracts/fixtures/open-request.json). `context.session_id` — UUID сессии Platform, верхнеуровневый `session_id` — ID CRM. Попытка определяется полным кортежем project/run/session/attempt. ID CRM уникален для привязки. Запрос открытия принимает до 4 MiB.

| Условие | Результат |
|---|---|
| Первое открытие | HTTP 201, `status=initialized`, контекст, ID CRM, текущий снимок |
| Повтор с теми же начальными данными | HTTP 200 и текущий снимок, включая уже созданные заявки |
| Другой payload для той же попытки или повторное использование CRM ID в другом контексте | HTTP 409 |
| Неверный токен | HTTP 401 |
| Недопустимый контекст, неподдерживаемые начальные данные или правило сбоя | HTTP 400 |

Отсутствующее или `null` поле clients/slots включает стандартную выборку для соответствующего поля. Пустой массив задаёт пустую выборку. Отпечаток исходного типизированного запроса сохраняет это различие и порядок элементов массива. Порядок ключей JSON и пробелы значения не имеют.

Для каждого CRM-вызова и служебной операции Runner добавляет три заголовка. Пример запроса после открытия попытки:

```http
GET /api/v1/orders HTTP/1.1
Authorization: Bearer <RUNNER_ENVIRONMENT_TOKEN>
X-Session-ID: crm-1
X-Execution-Context: {"project_id":"00000000-0000-4000-8000-000000000001","run_id":"00000000-0000-4000-8000-000000000002","session_id":"00000000-0000-4000-8000-000000000003","attempt_no":1}
```

ID в session-prefixed пути и параметре `session_id`, если они присутствуют, должны совпадать с заголовком. Чужая привязка возвращает 403. В интеграционном режиме открытие выполняется только через attempts. Попытка закрывается для записи с началом финализации. После этого create/update/cancel, настройка сбоев и reset возвращают 409. Авторизованные GET state/audit остаются доступны. Старые правила CRM, включая сохранение перед HTTP 504, продолжают действовать.

## RPC для Platform и Runner

[EnvironmentService](../contracts/proto/poligon/environment/v1/environment.proto) импортирует [Control v1](../contracts/proto/poligon/control/v1/control.proto) из опубликованного Platform. Запросы используют gRPC metadata `authorization: Bearer ...`.

| Метод | Кто вызывает | Что проверяет или возвращает Environment |
|---|---|---|
| `ValidateTemplate` | Platform | Принимает полный scenario через Struct, проверяет свою часть definition. Возвращает `errors` с path/code/message и `warnings`. Проверяются версия, текстовая модальность, форма clients/slots, уникальность ID и поддержка fault_rules. Начальные orders отклоняются. |
| `GetCapabilities` | Platform | Проверяет kind/version_id/modality и вложенный scenario. Возвращает supported, ошибки и SHA-256 опубликованной схемы финализации. |
| `FinalizeAttempt` | Runner | Принимает context и environment_session_id под сервисным токеном Runner. Возвращает тот же context/id, два ArtifactRef и оба флага полноты после регистрации. |

Полное имя сервиса — `poligon.environment.v1.EnvironmentService`. Профиль валидаторов Platform — `struct-proposal`. Поддерживается запись каталога Platform `a0000000-0000-4000-8000-000000000001`, `field-service/v1`, `field-service.v1`. Общий прогон с simulator/evaluator требует согласованных версий соседних сервисов.

Финализатор закрывает приём мутаций и ждёт выхода ранее принятых HTTP-обработчиков, включая задержку и запись события 504. Он фиксирует JSON со StateSnapshot и NDJSON с исходными AuditEvent. Последняя строка — `FINALIZE_ATTEMPT`, её `after_state` соответствует [схеме финализации](../contracts/schemas/finalization.json). В ней находится SHA-256 точных байтов снимка. HTTP audit по-прежнему выдаёт объект с session_id, events и count.

Каждый артефакт проходит PrepareArtifactUpload → подписанный PUT → RegisterArtifact. Оба запроса в Control выполняются от producer `environment`. Проверяются вид, MIME, UUID, размер и SHA в ответе. Ошибка сети или хранилища возвращается как ошибка RPC. Повтор использует замороженные байты, старый upload_id и метаданные незавершённой регистрации. Истечение execution lease проверяет Platform. Повтор успешно завершённого RPC читает сохранённые refs. Один вызов Finalize ограничен 30 секундами и deadline вызывающего сервиса.

CRM, привязки попыток и промежуточные данные повтора хранятся в памяти процесса. При его перезапуске Runner должен обрабатывать потерю активной сессии как сбой исполнения. Зарегистрированные объекты сохраняются в Platform и хранилище.

## Контракты, примеры и проверки

В [fixtures](../contracts/fixtures) лежат запросы и ответы, пример отказа при несовпадении context, JSON snapshot и исходный NDJSON audit. Фикстуры финализации представляют один тестовый обмен. Размеры и SHA в примере ответа соответствуют байтам файлов. ID артефактов в примере принадлежат тестовому реестру. JSON-представление ArtifactRef использует строку для size_bytes. Хеши зависимостей и фактические проверки перечислены в [integration-status.json](integration-status.json).

```sh
go test -race -count=1 ./...
go vet ./...
```

Для проверки реального стыка нужны тестовый PostgreSQL с правом создать временную БД и приватный MinIO с правом создать временный bucket. Передайте их параметры через `TEST_DATABASE_URL`, `TEST_S3_ENDPOINT` (host:port), `TEST_S3_ACCESS_KEY`, `TEST_S3_SECRET_KEY`. Из каталога компонента выполните:

```sh
./test/live/control.sh
```

Скрипт читает закреплённый SHA Platform из Git-объектов локального клона (`PLATFORM_REPO` позволяет задать другой клон). Во временном каталоге он собирает сервер Environment и проверку по исходникам Platform. Настоящий клиент валидатора вызывает оба RPC, тестовая обвязка Platform создаёт контекст исполнения, а клиент Runner вызывает открытие, CRM и финализацию по HTTP/gRPC. Проверяются приём двух refs, чтение объектов из MinIO по размеру и хешу, повтор RPC и отказ при просроченном lease. Проверка удаляет собственную тестовую БД и bucket. Обвязка создания задания использует тестовые валидаторы Platform, полный цикл оценки с Intelligence и диспетчеризацией Runner в эту проверку не входит.

Сгенерированные Go bindings включены в коммит. Для повторной генерации используйте protoc 31.1, protoc-gen-go v1.36.8 и protoc-gen-go-grpc v1.5.1 в PATH, затем `./contracts/generate.sh`. Потребитель с собственными bindings Control может сгенерировать Environment из тех же IDL с Go import mapping на свою каноническую копию Control.
