# Конфигурация Runner

Runner читает переменные окружения процесса. [.env.example](../.env.example) служит шаблоном. Пустое значение в столбце «По умолчанию» означает, что настройка не задана.

| Переменная | Назначение | По умолчанию | Обязательна |
|---|---|---|---|
| `RUNNER_MODE` | Профиль `local` или `production` | `local` | Нет |
| `RUNNER_NATS_URL` | Адрес брокера без учётных данных и пути | `nats://127.0.0.1:4222` | Да в production, со схемой `tls://` |
| `RUNNER_NATS_USER` | Пользователь NATS | Пусто | Вместе с паролем при аутентификации NATS |
| `RUNNER_NATS_PASSWORD` | Пароль пользователя NATS | Пусто | Вместе с именем пользователя |
| `RUNNER_DURABLE` | Имя общего durable consumer. От 1 до 80 букв ASCII, цифр, дефисов или подчёркиваний | `poligon-runner-v1` | Нет |
| `RUNNER_CONTROL_ADDR` | Адрес gRPC Control Platform | `127.0.0.1:9090` | Нет |
| `RUNNER_CONTROL_TOKEN` | Bearer token Runner для Control | Пусто | Да в local без mTLS. В production оставьте пустым |
| `RUNNER_TLS_CA` | Путь к файлу CA для проверки Control | Пусто | Да для mTLS, в том числе в production |
| `RUNNER_TLS_CERT` | Путь к клиентскому сертификату Runner | Пусто | Да для mTLS |
| `RUNNER_TLS_KEY` | Путь к ключу сертификата Runner | Пусто | Да для mTLS |
| `RUNNER_TLS_SERVER_NAME` | Имя сервера для проверки сертификата Control | Пусто | При необходимости переопределить имя из адреса |
| `RUNNER_ENVIRONMENT_URL` | Базовый URL Environment без query | `http://127.0.0.1:8081` | Да в production, со схемой `https://` |
| `RUNNER_UPLOAD_HOSTS` | Разрешённые адреса подписанного PUT. Список `host[:port]` через запятую | `localhost:9000,127.0.0.1:9000` | Да в production |
| `RUNNER_AGENT_HOSTS` | Разрешённые имена хостов агента через запятую | `localhost,127.0.0.1` | Да в production |
| `RUNNER_HTTP_JSON_PROPOSAL` | Разрешить адаптер `http-json-v1` | `false` | Для текстового адаптера |
| `RUNNER_SLOTS` | Параллельные попытки на экземпляр. От 1 до 256 | `8` | Нет |
| `RUNNER_MAX_ACK_PENDING` | Неподтверждённые задания на durable. От 1 до 8192, не меньше числа слотов | `128` | Нет |
| `RUNNER_HTTP_CONNECTIONS` | Лимит HTTP-соединений на хост в каждом пуле. От 1 до 512 | `16` | Нет |
| `RUNNER_MAX_PLAN_BYTES` | Максимальный размер плана в байтах. От 4 КиБ до 4 МиБ | `1048576` | Нет |
| `RUNNER_MAX_MESSAGE_BYTES` | Максимальный размер сообщения адаптера в байтах. От 1 до 64 КиБ | `65536` | Нет |
| `RUNNER_MAX_EVIDENCE_BYTES` | Бюджет журналов попытки в байтах. От 64 КиБ до 512 МиБ | `16777216` | Нет |
| `RUNNER_RPC_TIMEOUT` | Таймаут вызова Control. До 8 с | `3s` | Нет |
| `RUNNER_AGENT_TIMEOUT` | Таймаут запроса к агенту. До 30 с, план может уменьшить лимит | `15s` | Нет |
| `RUNNER_TOOL_TIMEOUT` | Таймаут HTTP-вызова Environment. До 30 с | `5s` | Нет |
| `RUNNER_UPLOAD_TIMEOUT` | Таймаут подписанного PUT. До 20 с | `6s` | Нет |
| `RUNNER_DRAIN_TIMEOUT` | Срок ожидания активных слотов перед отменой при остановке. До 2 мин | `20s` | Нет |
| `RUNNER_TEMP_DIR` | Каталог журналов попыток, доступный на запись | Системный временный каталог. В образе `/var/lib/runner` | Нет |
| `RUNNER_DIAGNOSTICS_ADDR` | Адрес HTTP-диагностики | `127.0.0.1:8091` | Нет |
| `RUNNER_DIAGNOSTICS_TOKEN` | Bearer token для метрик и pprof | Пусто | Для pprof и метрик вне loopback |
| `RUNNER_PPROF` | Включить pprof на loopback | `false` | Нет |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Общий endpoint для экспорта трассировок OTLP HTTP | Пусто | Нет |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Endpoint экспорта трассировок с приоритетом над общим | Пусто | Нет |

Минимальный таймаут во всех пяти настройках — `100ms`. Для mTLS задайте CA, сертификат и ключ вместе. В production укажите непустые списки разрешённых хостов и используйте TLS для брокера и HTTP-интеграций. Формат удостоверения Runner описан в [контракте Platform](integration.md#platform).
