# Контракты и готовность интеграций

## Версии

| Владелец | Зафиксированный источник | Состояние |
|---|---|---|
| Platform | [`2fe2700620c946e236377c79886e511743d7608c`](https://github.com/aVoKaD-0/Poligon/tree/2fe2700620c946e236377c79886e511743d7608c/platform) | Скопированы upstream protobuf, Go bindings, схемы и fixture manifest |
| Environment | [`c9932fea7eca48d6fccb2e06a997478c8c59da62`](https://github.com/aVoKaD-0/Poligon/tree/c9932fea7eca48d6fccb2e06a997478c8c59da62/environment) | Инициализация и восемь операций CRM поддержаны |
| Intelligence | [`ba3fd808abce70226eabed9ffac650f0c8ad1780`](https://github.com/aVoKaD-0/Poligon/tree/ba3fd808abce70226eabed9ffac650f0c8ad1780/intelligence) | Есть описание роли, API симуляции и сервис оценки отсутствуют |

Тест с `POLIGON_PLATFORM_TEST_DIR` сверяет содержимое контрактов с checkout Platform после нормализации переводов строк. Имя Go-модуля Runner не меняет protobuf package, идентификаторы RPC или поля сообщений.

## Platform

В локальной среде Control принимает `authorization: Bearer <runner-token>`. Для mTLS у сертификата Runner требуется URI SAN `spiffe://poligon/runner`, а сертификат Platform проверяется по доверенному CA и имени сервера. JetStream использует `POLIGON_EXECUTION` и `POLIGON_EVENTS`, созданные Platform. Все реплики одного deployment делят имя durable и настройки AckWait и MaxAckPending.

Задания поступают по subject `poligon.cmd.session.execute.v1`. События используют `poligon.evt.session.{started,progress,executed,failed,cancelled}.v1`. Envelope включает причинный ID команды, корреляционный ID запуска и W3C traceparent. Прогресс несёт тот же ID и порядковый номер, что и запись NDJSON. Platform проверяет scope, worker и актуальность lease. Publish Ack от JetStream подтверждает запись в брокере. Inbox Platform подтверждает применение позже.

При загрузке Runner передаёт подписанные Content-Type, Content-Length и X-Amz-Checksum-Sha256. Адрес подписанного PUT сверяется со списком `RUNNER_UPLOAD_HOSTS`. Повтор Register использует исходные метаданные и upload ID. Manifest отправляется только после проверки ссылок обоих владельцев. Неуспешные терминальные события содержат `evidence_refs` при наличии частичных доказательств. Platform фиксирует этот payload в брокере, но текущий HTTP-отчёт не проецирует ссылки из `session.failed.v1` и `session.cancelled.v1`.

Для полной интеграции владельцу Platform нужно включить provider-owned validators либо локальный `VALIDATOR_WIRE_MODE=struct-proposal` с настоящими поставщиками validation. Без них публикация сценария и создание запуска заблокированы upstream. Для просмотра частичных материалов в UI требуется отобразить проверенные refs неуспешных событий в Platform.

## Environment

Runner использует `POST /api/v1/sessions` и `X-Session-ID` с идентификатором области выполнения, включающим номер попытки. Пустые и отсутствующие fixture-коллекции сохраняют смысл из upstream API. Каталог фиксирован. Служебные маршруты и произвольные URL через tool calls недоступны.

| Имя | Аргументы |
|---|---|
| `list_clients` | Пустой объект |
| `get_client`, `get_order`, `cancel_order` | `id` |
| `search_slots` | Необязательные `date`, `service`, `available` |
| `list_orders` | Необязательные `client_id`, `status` |
| `create_order` | `client_id`, `address`, `slot_id` либо `date` и `time_window`. Необязательные `service_type`, `notes` |
| `update_order` | `id` и хотя бы одно поле из `address`, `date`, `time_window`, `slot_id`, `notes` |

Idempotency key передаётся без изменения в заголовке мутации. В закреплённом Environment ключ учитывает только создание заказа. Runner не переотправляет мутации при сетевой ошибке и передаёт HTTP 504 после сохранения как фактический ответ CRM.

**Предложение финализации v1 для владельца Environment.** Приватный аутентифицированный вызов `FinalizeAttempt` принимает ExecutionContext и идентификатор изолированной CRM-сессии. Владелец Environment закрывает приём новых операций в этой попытке, дожидается уже начатых мутаций, фиксирует согласованные snapshot и audit, регистрирует их в Platform от сервисной идентичности Environment и возвращает два неизменных ArtifactRef. Повтор с тем же scope возвращает исходные refs. Срок хранения и удаление состояния принадлежат Environment. В закреплённой версии Environment отсутствуют удаление сессии по HTTP, защита доступа по scope, регистрация артефактов и отмена искусственных задержек по request context. Для совместной эксплуатации нужен приватный защищённый транспорт с авторизацией. Сейчас `Finalize` возвращает ошибку отсутствующего контракта.

## Intelligence

**Предложение v1 для владельца Intelligence.** Опубликовать аутентифицированные операции `OpenSimulation`, `NextClientAction` и `CloseSimulation`. Open получает execution scope, seed, simulator definition и сведения клиента из сценария. Next получает непрозрачный идентификатор симуляции, номер шага и ограниченный набор реплик последнего ответа. Возвращает либо `say` с текстом, либо `finish`. Нужны идемпотентная инициализация и следующий шаг по паре simulation ID и step, дедлайн, явно заявленная поддержка seed и версионирование возможностей scripted/model-generated. Оценщик должен обрабатывать отдельное назначение из Platform по зарегистрированному manifest.

Runner имеет интерфейс `SimulatorFactory`. Текущий бинарник Runner запускается без клиента Intelligence. Тестовый scripted simulator находится в Go-тестах. При наличии voice API надо согласовать аудиодействия, передачу чанков, бардж-ин, временную шкалу и конечные сигналы.

## Предложение wire-протокола агента v1

### `http-json-v1`

Явное включение `RUNNER_HTTP_JSON_PROPOSAL=true` разрешает проверенный в тестах формат. Клиент выполняет JSON POST по единственному зарегистрированному endpoint. Runner создаёт непрозрачный случайный `session_id` для агента. При наличии runtime credential передаёт её как Bearer token только этому endpoint. Redirect, credentials в URL и query запретны.

Каждый запрос содержит `schema_version: 1`, `session_id` и `action`. Допустимы `open`, `exchange`, `close`. Для `exchange` поле `input` содержит `message` или массив `tool_results`. Остальные операции идут без `input`.

Ответ HTTP 200 возвращает те же три поля. Для `exchange` обязательна `reply` с полями `messages`, `tool_calls`, `finished`, `cost_minor` по необходимости. Вызов инструмента содержит уникальный в сессии `id`, `name`, объект `arguments` и необязательный `idempotency_key`. Результат содержит `id`, при наличии фактический `http_status`, тело CRM в `body` и код локальной ошибки в `error`. `finished` завершает сессию и несовместим с запросом инструментов. Суммарный текст одного ответа ограничен 24 КиБ, в ответе допускается до 16 сообщений и 16 вызовов. `cost_minor` представляет неотрицательную стоимость конкретного обмена, которая накапливается за сессию. Отсутствующая стоимость остаётся неизвестной или частичной.

Имена полей чувствительны к регистру. В wire JSON допускается до 64 уровней вложенности и 512 полей в объекте. Строки с символом NUL отклоняются, поскольку Platform сохраняет доказательства в JSONB. Если несколько ответов CRM вместе превышают лимит wire-сообщения, Runner опускает минимальное число тел и помечает эти результаты `response_too_large`. Порядок, идентификаторы и полученные HTTP-статусы сохраняются. Тела остаются в журнале в пределах его лимита. Если даже метаданные результатов не поместятся, Runner завершает сессию до отправки этого пакета вызовов в CRM.

### `ws-pcm-v1`

Реализован проверяемый транспорт предложения с subprotocol `poligon.ws-pcm.v1`. Начальный текстовый JSON frame содержит `schema_version: 1`, `type: open` и `format` с `encoding: s16le`, частотой 8000–48000 Гц и 1–2 каналами. Ответ `accepted` повторяет формат. Дальше идут ограниченные по длине binary frames со строго целым числом семплов. Чтение и запись работают с контекстом и тайм-аутом. Проверено согласование формата и двунаправленная передача.

Подключение этого транспорта к голосовому сценарию, аудиотреки, alignment и прерывания зависят от утверждённого контракта с агентом и Intelligence. Runner сообщает об отсутствующей поддержке голосового диалога через типизированную ошибку интеграции.
