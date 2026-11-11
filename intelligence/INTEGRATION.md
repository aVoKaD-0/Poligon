# Точки интеграции для backend

Это предложение provider-owned v1. Реализованный Python gRPC проверен локальным
Python-клиентом. Подключения к production Runner, Control, брокеру и artifact registry
пока нет. Исходные версии и общие схемы: `REPOSITORY_REVIEW.md`, `contracts/upstream.lock.json`.

## gRPC и Runner

«grspc» вероятнее всего означает gRPC. В ветках Platform и Runner уже есть
`poligon.control.v1.ControlService`. Его контракт скопирован без изменения. Новый
`poligon.intelligence.v1.IntelligenceService` дополняет его; это отдельный сервис.
Runner предложил Open/Next/Close, поэтому использованы эти имена, а не введён
несовместимый GenerateClientTurn. EvaluateSession поставляет assessment.

1. Runner реализует свой `SimulatorFactory` клиентом нашего proto.
2. OpenSimulation получает ExecutionContext, request UUID, seed, mode и frozen Scenario.
   Сценарий размещается в simulator definition либо в
   `client_known_facts.intelligence_scenario` через мост ниже. Передавать весь runtime
   plan с секретами нельзя. Сценарий уже содержит initial/goal/constraints/limits.
3. NextClientAction получает scope, simulation UUID, step (с нуля), request UUID и
   последние agent_messages. История клиента сохраняется внутри симуляции.
4. Ответ say содержит текст; finish завершает диалог. metadata.validity=invalid
   означает ошибку симуляции: Runner должен передать invalid в материалы оценки,
   а не зарегистрировать дефект агента. Остановку из-за отсутствия вопроса симулятор
   делает явно, не раскрывая скрытое уточнение самостоятельно.
5. CloseSimulation идемпотентен, запрещает новые шаги. Повтор уже выданного шага
   возвращает исходный ответ. Смена scope (включая attempt) не даёт доступ к симуляции.

Идентичный Open по scope возвращает тот же ID. Другое определение в той же попытке —
ALREADY_EXISTS. Next идемпотентен по scope/simulation/step, даже если request UUID
сменился. Отличающийся payload на том же шаге — ALREADY_EXISTS. Ошибка модели
фиксируется как terminal invalid_simulation. Недоступный provider — инфраструктурная
ошибка, состояние не продвигается. Evaluate идемпотентен по scope/evaluation_id;
новая версия материалов или quality settings требует нового evaluation_id.

SQLite хранит и состояние, и ответы в одной транзакции. После перезапуска повтор
возвращает те же байты результата. Несколько процессов с одной локальной БД
сериализуют записи. Модельный вызов держит транзакцию: это ограничение пропускной
способности MVP, не масштабируемая очередь. Не использовать SQLite на сетевом диске.
Автоматическое удаление записей не реализовано: срок повторов/retention надо согласовать.

Обязателен deadline <=30 секунд. Дедлайн/отмена проверяются до и после вычисления,
перед commit. Provider получает остаток времени; уже начатый внешний вызов может
продолжаться до его ограниченного timeout. Отмена после commit не откатывает
выданный результат: повтор получает сохранённый ответ. Для новой операции нужен
новый ID. Исполнение мутаций CRM и их идемпотентность остаются у Runner/Environment.

Сервис по умолчанию только на loopback. Bearer token приходит через metadata
`authorization`. `--insecure-local` — явное включение демо без токена. Сервер не
открывает внешний plaintext порт. Для совместного deployment нужны mTLS, привязка
сервисной идентичности к project scope и политика доступа; local token сам по себе
не доказывает многопользовательскую авторизацию.

## Platform и валидация

ValidateScenario/GetCapabilities реализуют opt-in `struct-proposal` из Platform:
`google.protobuf.Struct`, пути RPC совпадают с RemoteValidators. Это не заменяет
проверки Environment и не снимает необходимости утвердить API. Поддерживаются
только text и явные MVP версии, перечисленные через `scripts/export_platform_scenarios.py`.
Неизвестные версии/voice отклоняются. Дополнительное определение Scenario лежит в
client_known_facts.intelligence_scenario, его goal/client/initial_fixture/fault_rules
сверяются с внешними полями. Platform scenario schema при экспорте тоже проверяется.

Для полноценного evaluation worker следующий шаг команды:

- получить `poligon.cmd.evaluation.execute.v1`, вызвать ClaimEvaluation;
- поддерживать HeartbeatEvaluation и cancel_requested;
- проверить frozen manifest, scope, evaluator definition/version/hash;
- ResolveArtifact с lease, загрузить зарегистрированные байты, проверить SHA/size;
- подготовить SessionMaterials, вызвать функцию evaluate либо локальный RPC;
- зарегистрировать assessment через PrepareArtifactUpload/RegisterArtifact;
- отправить `evaluation.completed.v1` с действительными ref/checksum и подтвердить
  сообщение только после надёжного результата. Ошибки стенда отправлять по lifecycle
  Platform, не подменять ими verdict=fail.

Эти шаги здесь описаны, а не реализованы. Прямой RPC нельзя подключить вместо NATS
worker без изменения orchestration. manifest_sha256 в локальном запросе проверяется
по форме, но сам manifest/registry отсутствует: его подлинность должен подтвердить
адаптер Platform. Отправитель локального RPC считается доверенным тестовым клиентом.

## Факты Environment и доказательства

Environment владеет snapshot/audit и сбоями. fault_rules Intelligence только описывает.
Snapshot читается в фактическом формате ветки Environment: session_id, clients/slots/
orders (maps), active_orders_count, total_orders_count. Счётчики пересчитываются.
Audit — объект `{session_id, events:[...]}` с независимыми response_status и after_state:
HTTP 504 после сохранения сам по себе не является инфраструктурной ошибкой стенда.

Материалы содержат точную UTF-8 строку артефакта и SHA-256 от её байтов. ID артефакта
и JSON Pointer сохраняются в каждом assertion. rule_id заканчивается `.v1`, поскольку
upstream assertion запрещает дополнительное поле rule_version. expected/actual/reason
соответствуют upstream, applicability.layer разделяет facts/behavior.

Для локального журнала нужен JSON-массив Runner envelope:
project/run/session/attempt, producer_event_id, session_seq, event_kind, payload.
Повтор идентичного producer_event_id игнорируется, конфликт, другой scope и перестановка
считаются проблемой доказательств. Поле occurred_at из Runner сохраняется. Runner
выдаёт session_seq также live lifecycle событиям, поэтому разрывы номеров в двух
artifact журналах допустимы; потеря проверяется по expected_session_seqs, если
доверенный адаптер передал отдельный индекс покрытия. JSON Pointer указывает
на исходный индекс массива, в том числе при повторной доставке.

Реальный Runner пишет несколько NDJSON-журналов, а не наш JSON-массив. Перед общей
приёмкой согласовать нормализацию: объединить conversation/tool_calls по session_seq,
сохранить оригиналы и зарегистрировать нормализованный массив отдельным артефактом
с явно согласованным producer/kind. Выдумывать registry UUID или ссылаться на массив
под SHA исходного NDJSON нельзя. Поэтому сборщик production artifacts здесь отсутствует.

snapshot_finalized, audit_complete и journal_complete — утверждения доверенного
адаптера, а не вывод LLM. Environment пока не имеет согласованного FinalizeAttempt.
Один GET state во время мутаций не даёт основания ставить finalized=true.
environment_session_id должен быть связан с изолированной CRM-попыткой; snapshot и
audit сверяются с ним, журнал сверяется с полным ExecutionContext.

## Статусы и уровни оценивания

| Категория в completeness.category | verdict | simulation_validity |
|---|---|---|
| success | pass | valid |
| agent_defect | fail | valid |
| infrastructure_error | inconclusive | исходное значение |
| insufficient_evidence | inconclusive | valid/unknown |
| invalid_simulation | inconclusive | invalid |

Приоритет: ошибка доказательств/инфраструктуры, некорректная симуляция, недостаток
снимка/валидности, подтверждённые нарушения обязательных правил, затем полнота
остальных обязательных проверок. Наблюдаемые нарушения сохраняются и при invalid,
но не попадают в группировку дефектов агента. Нет снимка — нет фактического verdict=fail.

Обязательные фактические правила проверяют число активных заявок только нужного
клиента, дубликаты, адрес/дату/время каждой подходящей заявки, сохранение ID при
изменении, отмену нужной заявки, защищённые поля/чужие заявки/клиентов. Полный audit
нужен, чтобы видеть промежуточные запрещённые изменения, даже если они восстановлены.

Behavior v1 — ограниченные объяснимые эвристики: вопросительный знак при неоднозначности,
чтение состояния/ключ при повторе после 504, подтверждение до успешного tool result.
Они advisory (required=false): текстовый шаблон и один успешный вызов ещё не доказывают
смысловую корректность. Не выдаём эти эвристики за полноценную семантическую проверку.
Субъективные clarity/coherence/appropriateness — отдельные scores 1..5 с моделью,
настройками, prompt_version, rubric и доказательствами. Без LLM scores пусты с явным
quality=not_evaluated. Ошибка LLM не отменяет проверку БД.

## Что согласовать

RPC/provider version UUID, допустимые определения simulator/evaluator; нормализацию
Runner NDJSON и разрешённый producer нормализованного артефакта; Environment finalization
и scope authorization; порядок и полноту audit; ownership business assertions;
модель угроз, mTLS, retention и лимиты; bootstrap существующих заявок (Runner и
Environment сейчас принимают только clients/slots); broker worker и mapping lifecycle; реальный
набор независимых человеческих меток и скрытые семейства для итоговой оценки.
