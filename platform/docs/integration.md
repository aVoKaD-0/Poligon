# Интеграция Platform

## Границы

Публичный HTTP/SSE и Control gRPC поставляет Platform. Общие контракты этого сервиса размещены внутри `platform/contracts`; чужие таблицы и бизнес-пакеты не используются. Runner, Environment и Intelligence должны согласовать validation subset и artifact schemas до сквозной приёмки.

Исходные ТЗ находятся в корневой папке проекта; backend 1 их не изменяет. `contracts/proto/validation.proto` описывает предложение wire-формата `google.protobuf.Struct` для ещё не опубликованных validators. Это открытая интеграционная зависимость. Временный Struct adapter включается только явным VALIDATOR_WIRE_MODE=struct-proposal; без него публикация сценария и создание нового run возвращают 503. При появлении provider-owned Protobuf его клиент заменяется за интерфейсом `Validators`.

## Control

Package: `poligon.control.v1`, service: `ControlService`.

В local отправлять metadata `authorization: Bearer SERVICE_TOKEN`. Production проверяет клиентский сертификат доверенного CA с URI SAN `spiffe://poligon/runner`, `spiffe://poligon/environment` или `spiffe://poligon/intelligence`.

Все запросы содержат `ExecutionContext`: project/run/session UUID и attempt_no >= 1. Worker UUID задаётся самим исполнителем и сохраняется при claim.

1. Runner получает `dispatch_id`, `runtime_plan_ref` и `runtime_plan_sha256` из команды.
2. `ClaimExecution` с context/worker/dispatch выдаёт lease на 30 секунд, frozen plan и разрешённые runtime credentials. Повтор тем же worker сохраняет lease. Другой worker получает `ALREADY_CLAIMED`.
3. `HeartbeatExecution` каждые 10 секунд с lease_id. `cancel_requested=true` означает прекратить новые действия и завершить финализацию. Отказ/expiry запрещает новые действия.
4. `GetRuntimePlan` проверяет worker, lease и ref/hash. Credentials не сохраняются в плане или брокере.
5. Intelligence аналогично вызывает `ClaimEvaluation`/`HeartbeatEvaluation`, указывая evaluation_id. Он получает назначенную evaluator definition отдельно от исходного плана.

`CAPACITY_EXCEEDED` оставляет durable job ожидающим; новый attempt из-за квоты не создаётся. Срок job ограничивает heartbeat. Старые execution attempts и evaluation retries не заменяют текущий результат.

## Артефакты

`PrepareArtifactUpload` выдаёт upload_id, серверный object_key, URL и required_headers на 10 минут. Обязательно отправлять все required_headers; размер и checksum связаны с grant. PUT идёт в staging, а не в постоянный object_key. Затем `RegisterArtifact` с исходными метаданными проверяет SHA-256 по фактическим байтам и копирует проверенный объект в постоянный namespace.

Повтор регистрации upload_id возвращает тот же ArtifactRef; изменённые метаданные — conflict. Использованный PUT URL не позволяет изменить зарегистрированный объект. ArtifactRef в JSON использует строковый size_bytes; в Protobuf это int64.

Producer prefixes: runner/, environment/, intelligence/{evaluation_id}/ внутри project/run/session/attempt. Environment пишет только snapshot/audit и свои diagnostics. Intelligence пишет assessment/diagnostics только назначенного evaluation. Resolver проверяет действующий lease, context и доступный producer/kind. Browser download сначала проверяет membership и выдаёт URL на 60 секунд.

Runner публикует manifest после регистрации всех refs. Manifest и assessment валидируются по JSON Schema. JSON manifest/assessment ограничены 16 MiB для разбора; общий лимит объекта 512 MiB. Conversation/tool-call журналы NDJSON содержат context, producer_event_id, session_seq и payload; событие не больше 64 KiB. По этим файлам восстанавливаются пропущенные live events. Voice refs сохраняются без интерпретации аудиобайтов Platform.

## Брокер

Команды: `poligon.cmd.session.execute.v1` и `poligon.cmd.evaluation.execute.v1` в отдельных WorkQueue streams. Results: `poligon.evt.session.*.v1` и `poligon.evt.evaluation.*.v1` в Limits stream. Все участники используют envelope из общего ТЗ.

Обязательны schema_version=1, UUID message_id/producer_event_id/project_id/run_id/session_id, attempt_no, producer, type, occurred_at и payload. У всех session events требуется session_seq, у evaluation events — evaluation_id. Для executed/completed refs и checksum обязательны и сверяются с registry.

Accepted terminal event фиксируется вместе с inbox, изменением состояния, SSE и следующей outbox-командой. Ack после commit. Повтор не создаёт вторую оценку. Некорректное событие или исчерпание восьми доставок фиксируется в quarantine и operator log. Ошибки кода поведения агента не вызывают новый разговор; инфраструктурный retry разрешён только для явного whitelist error codes.

Retryable execution codes: LEASE_EXPIRED, INFRASTRUCTURE_ERROR, TRANSPORT_ERROR, ARTIFACT_UPLOAD_FAILED, DEPENDENCY_UNAVAILABLE. Максимум две execution attempts и три jobs оценки. QUEUE_TIMEOUT после 24 часов завершает работу явно.

## Публичный клиент

HTTP root: `/api/v1`. Списки `{items,next_cursor}`; limit 1–100. Создание run, retry и reevaluate требуют Idempotency-Key со сроком 24 часа. Повтор исходного запроса работает при временно недоступном validator. Draft/project/resource PATCH требуют If-Match с quoted revision. Публикация тоже требует актуальную revision, чтобы результат внешней validation не применился к изменённому draft.

Seed ограничен безопасными JSON integers ±(2^53−1) для JCS и браузерного клиента. Внешние UUID и ссылки на секреты проверяются в project context.

SSE: сначала RunView.last_event_cursor, затем `/runs/{run_id}/stream?after_cursor=...`. Last-Event-ID имеет приоритет. Cursor и revision — десятичные строки. На stream.reset перечитать snapshot. Heartbeat — отдельное событие без id каждые 15 секунд. Журнал сессии использует after_seq и attempt_no, а не cursor списка UUID.

Manual evaluation создаёт отдельную серию; primary status/verdict/report/comparison не меняются. Comparison требует совпадающих scenario versions, seeds, repeats, environment/simulator/evaluator. Изменение порядка списка сценариев допустимо. Технические/неопределённые случаи показываются как excluded, а не improved.
