# Проверка Runner

## Результаты

Проверка проведена на Windows amd64 с Go 1.27.2. Внешний Environment собран Go 1.27.3 из закреплённой ветки.

| Проверка | Результат |
|---|---|
| `go test -count=1 ./...` | Прошёл с реальным Environment и проверкой контрактов и ACL Platform |
| `go test -race -count=1 ./...` | Прошёл с теми же интеграциями и LLVM MinGW UCRT |
| `go vet ./...`, `go build ./...`, `go mod verify` | Прошли |
| JetStream 2.11.9 и ACL Platform | 20 параллельных слотов, восстановление после перезапуска брокера, redelivery после drain, PubAck и AckSync |
| Реальный Environment | HTTP 504 после записи одной заявки, идемпотентный повтор и изоляция следующей попытки |
| Runner → Platform → PostgreSQL 17.6 → MinIO `RELEASE.2025-09-07T16-13-09Z` | Реальные claim, heartbeat, JCS-проверка frozen plan, подписанные PUT, регистрация и ResolveArtifact. Проверены непустой и пустой журналы, а также отказ второму worker |
| Fuzz | 4 604 исполнения для envelope и 45 520 для маршрутов CRM за два прогона по 3 секунды. Падений нет |
| Диагностика запуска | HTTP 200 для livez, 503 для readyz с недоступными зависимостями, 401 для анонимного pprof, 200 и goroutine profile с токеном |

Dockerfile подготовлен. Сборка контейнера в этой среде не запускалась, поскольку Docker отсутствует.

## Fuzz-тесты

Базовые команды приведены в [руководстве по разработке](development.md#тестирование). Чтобы воспроизвести проверки декодирования задания и маршрутизации CRM, выполните из каталога Runner:

```sh
go test ./internal/execution -run '^## Настоящий Environment и снимок контракта Platform

Поднимите закреплённый Environment на локальном порту и укажите его адрес. В PowerShell:

```powershell
$env:POLIGON_ENVIRONMENT_TEST_URL = "http://127.0.0.1:18081"
$env:POLIGON_PLATFORM_TEST_DIR = "C:/checkouts/poligon-platform/platform"
go test -count=1 ./...
```

`TestRealEnvironmentTimeoutAfterSave` создаёт отдельную CRM-попытку, получает 504 от настоящего Environment и проверяет состояние через `list_orders`. Явный повтор с тем же ключом возвращает существующую заявку. Следующий attempt получает изолированную CRM.

## Настоящая Platform и MinIO

Стенд требует закреплённый checkout Platform, отдельный тестовый PostgreSQL с правом создавать базы и отдельный тестовый MinIO. В fixture Platform используются test-only validators. Overlay добавляет Go-тест в процесс сборки Platform и позволяет оставить исходники checkout на месте. Probe собирается из исходников Runner и вызывает настоящий Control по TCP. Сначала проверяются claim, heartbeat, ref и JCS SHA-256. Затем подписанные PUT отправляются в MinIO, а Platform проверяет фактические байты, регистрирует и разрешает ссылку.

```powershell
$runnerDir = (Get-Location).Path
$platformDir = "C:/checkouts/poligon-platform/platform"
$probeDir = Join-Path $env:TEMP ("poligon-runner-probe-" + [guid]::NewGuid())
New-Item -ItemType Directory $probeDir | Out-Null
$env:POLIGON_RUNNER_PROBE_BIN = Join-Path $probeDir "runner-probe.exe"
go test -c -o $env:POLIGON_RUNNER_PROBE_BIN ./internal/platform

$replace = @{}
$replace[(Join-Path $platformDir "internal/platform/poligon_runner_probe_test.go")] = Join-Path $runnerDir "tests/platformfixture/runner_probe.go.txt"
$overlay = Join-Path $probeDir "overlay.json"
@{ Replace = $replace } | ConvertTo-Json -Compress | Set-Content -LiteralPath $overlay -Encoding utf8

$env:TEST_DATABASE_URL = "postgres://poligon_test@127.0.0.1:15432/postgres?sslmode=disable"
$env:POLIGON_MINIO_TEST_ENDPOINT = "127.0.0.1:19000"
# Задайте POLIGON_MINIO_TEST_ACCESS_KEY и POLIGON_MINIO_TEST_SECRET_KEY для тестового экземпляра.
Push-Location $platformDir
go test -overlay $overlay -v ./internal/platform -run '^TestPoligonRunnerLiveProbe$' -count=1
Pop-Location
```

## Замеры

```powershell
$env:GOMAXPROCS = "4"
$env:POLIGON_RUNNER_LOAD = "3s"
$env:POLIGON_RUNNER_SOAK = "45s"
go test -v ./internal/execution -run '^TestLoadSessions$' -count=1
go test -run '^$' -bench . -benchmem -benchtime 350ms ./internal/execution ./internal/transport
```

Нагрузочный тест использует управляемую задержку агента 40 мс, отдельный scope на слот и тестовые Control, Intelligence, CRM и object storage. В soak есть управляемые отмены и сбои загрузки с восстановлением в следующей сессии. Он проверяет, что после остановки нет активных сессий, оставшихся журналов или устойчивого прироста горутин. Бенчмарки без задержки агента измеряют накладные расходы Runner отдельно. Для pprof доступны `/debug/pprof/profile`, `heap`, `allocs`, `goroutine`, `block` и `mutex` при настроенной диагностической аутентификации.

Фактический прогон на Intel Core Ultra 5 125H с `GOMAXPROCS=4`. Память в таблице — Go heap in use. Числа не включают потребление памяти внешними сервисами и не представляют RSS процесса.

| Слотов | Сессий/с | p50 / p95 / p99, мс | CPU, с | Heap пик / после GC, МиБ | Горутин пик / после | Байт аллокаций на сессию | Drain, мс |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 22,5 | 43,5 / 45,2 / 49,3 | 0,25 | 3,94 / 2,69 | 6 / 3 | 115 506 | 28 |
| 10 | 185,2 | 53,7 / 63,5 / 69,3 | 3,96 | 4,46 / 2,97 | 24 / 3 | 114 081 | 45 |
| 50 | 395,1 | 129,4 / 158,5 / 169,1 | 8,59 | 6,81 / 3,56 | 104 / 3 | 115 722 | 108 |
| 100 | 387,4 | 253,2 / 306,9 / 329,1 | 8,55 | 9,67 / 4,48 | 204 / 3 | 118 985 | 138 |

На всех четырёх уровнях неожиданных ошибок и тайм-аутов не было. При переходе с 50 на 100 слотов throughput вышел на плато, а задержка выросла. Этот результат относится к локальному стенду с синтетическим агентом.

За 45 секунд soak с 20 слотами обработано 12 955 сессий, в том числе 554 управляемые отмены и 407 инъекций сбоев загрузки. Зарегистрировано 27 707 heartbeat и 0 неожиданных исходов. Heap составил 3,69 МиБ до прогона, 6,37 МиБ на пике и 4,01 МиБ после GC. Число горутин вернулось с пиковых 44 к исходным 3, журналы удалены. Последние операции завершились за 55 мс после прекращения подачи нагрузки.

CPU profile показал, что на этой Windows-системе около 90% выборки приходится на `runtime.cgocall`, через который проходят системные вызовы. В профиле аллокаций видны буферы NDJSON, разбор и валидация JSON. Block profile в основном показывает штатное ожидание монитора lease. Mutex profile преимущественно содержит блокировки Go runtime. Профили CPU, heap/allocs, block и mutex собраны на бенчмарках, goroutine profile снят с запущенного бинарника.

Буферы NDJSON уменьшены с 16 до 4 КиБ. Для журнала с секретом повторный JSON-разбор теперь нужен при возможном совпадении или наличии escape-последовательности. На тех же наборах бенчмарков общий объём аллокаций сократился с 175 до 142 кБ на одну сессию и со 187 до 155 кБ на сессию при 100 слотах. Ускорение из двух прогонов с разной нагрузкой хоста не выводилось.

Повторный замер без задержки агента дал 137 мкс и 907 аллокаций на валидацию frozen plan, 9,7 мкс и 20 аллокаций на событие NDJSON с публикацией в тестовый sink и 5,5 мкс и 42 аллокации на валидацию маршрута CRM. Метрики реального Runner отдельно учитывают время до claim, длительность ответа агента и длительность сессии.

## Сквозная приёмка

После публикации недостающих контрактов поднимите Platform с validators, Environment с финализатором и Intelligence с симулятором и evaluator. Запустите Runner и агента, поддерживающего согласованный wire v1. В Platform опубликуйте текстовый сценарий с первым POST `/api/v1/orders` под сбоем `timeout_after_save`. Запустите один attempt и проверьте наблюдаемую последовательность.

1. Platform создаёт задание, Runner берёт lease и появляется `session.started.v1`.
2. Intelligence задаёт запрос, агент вызывает `create_order`, а Runner передаёт ему исходный HTTP 504.
3. Environment регистрирует согласованные snapshot и audit. Проверяется количество заявок и история повторов агента.
4. Runner публикует `session.executed.v1` после регистрации manifest. Platform создаёт назначение на оценку.
5. Intelligence получает артефакты, публикует результат оценки, Platform показывает его в отчёте.

Полная цепочка заблокирована недостающими API, перечисленными в [integration.md](integration.md). Тестовые интеграции, описанные выше, проверяют отдельные границы.
 -fuzz '^FuzzDecodeJob## Настоящий Environment и снимок контракта Platform

Поднимите закреплённый Environment на локальном порту и укажите его адрес. В PowerShell:

```powershell
$env:POLIGON_ENVIRONMENT_TEST_URL = "http://127.0.0.1:18081"
$env:POLIGON_PLATFORM_TEST_DIR = "C:/checkouts/poligon-platform/platform"
go test -count=1 ./...
```

`TestRealEnvironmentTimeoutAfterSave` создаёт отдельную CRM-попытку, получает 504 от настоящего Environment и проверяет состояние через `list_orders`. Явный повтор с тем же ключом возвращает существующую заявку. Следующий attempt получает изолированную CRM.

## Настоящая Platform и MinIO

Стенд требует закреплённый checkout Platform, отдельный тестовый PostgreSQL с правом создавать базы и отдельный тестовый MinIO. В fixture Platform используются test-only validators. Overlay добавляет Go-тест в процесс сборки Platform и позволяет оставить исходники checkout на месте. Probe собирается из исходников Runner и вызывает настоящий Control по TCP. Сначала проверяются claim, heartbeat, ref и JCS SHA-256. Затем подписанные PUT отправляются в MinIO, а Platform проверяет фактические байты, регистрирует и разрешает ссылку.

```powershell
$runnerDir = (Get-Location).Path
$platformDir = "C:/checkouts/poligon-platform/platform"
$probeDir = Join-Path $env:TEMP ("poligon-runner-probe-" + [guid]::NewGuid())
New-Item -ItemType Directory $probeDir | Out-Null
$env:POLIGON_RUNNER_PROBE_BIN = Join-Path $probeDir "runner-probe.exe"
go test -c -o $env:POLIGON_RUNNER_PROBE_BIN ./internal/platform

$replace = @{}
$replace[(Join-Path $platformDir "internal/platform/poligon_runner_probe_test.go")] = Join-Path $runnerDir "tests/platformfixture/runner_probe.go.txt"
$overlay = Join-Path $probeDir "overlay.json"
@{ Replace = $replace } | ConvertTo-Json -Compress | Set-Content -LiteralPath $overlay -Encoding utf8

$env:TEST_DATABASE_URL = "postgres://poligon_test@127.0.0.1:15432/postgres?sslmode=disable"
$env:POLIGON_MINIO_TEST_ENDPOINT = "127.0.0.1:19000"
# Задайте POLIGON_MINIO_TEST_ACCESS_KEY и POLIGON_MINIO_TEST_SECRET_KEY для тестового экземпляра.
Push-Location $platformDir
go test -overlay $overlay -v ./internal/platform -run '^TestPoligonRunnerLiveProbe$' -count=1
Pop-Location
```

## Замеры

```powershell
$env:GOMAXPROCS = "4"
$env:POLIGON_RUNNER_LOAD = "3s"
$env:POLIGON_RUNNER_SOAK = "45s"
go test -v ./internal/execution -run '^TestLoadSessions$' -count=1
go test -run '^$' -bench . -benchmem -benchtime 350ms ./internal/execution ./internal/transport
```

Нагрузочный тест использует управляемую задержку агента 40 мс, отдельный scope на слот и тестовые Control, Intelligence, CRM и object storage. В soak есть управляемые отмены и сбои загрузки с восстановлением в следующей сессии. Он проверяет, что после остановки нет активных сессий, оставшихся журналов или устойчивого прироста горутин. Бенчмарки без задержки агента измеряют накладные расходы Runner отдельно. Для pprof доступны `/debug/pprof/profile`, `heap`, `allocs`, `goroutine`, `block` и `mutex` при настроенной диагностической аутентификации.

Фактический прогон на Intel Core Ultra 5 125H с `GOMAXPROCS=4`. Память в таблице — Go heap in use. Числа не включают потребление памяти внешними сервисами и не представляют RSS процесса.

| Слотов | Сессий/с | p50 / p95 / p99, мс | CPU, с | Heap пик / после GC, МиБ | Горутин пик / после | Байт аллокаций на сессию | Drain, мс |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 22,5 | 43,5 / 45,2 / 49,3 | 0,25 | 3,94 / 2,69 | 6 / 3 | 115 506 | 28 |
| 10 | 185,2 | 53,7 / 63,5 / 69,3 | 3,96 | 4,46 / 2,97 | 24 / 3 | 114 081 | 45 |
| 50 | 395,1 | 129,4 / 158,5 / 169,1 | 8,59 | 6,81 / 3,56 | 104 / 3 | 115 722 | 108 |
| 100 | 387,4 | 253,2 / 306,9 / 329,1 | 8,55 | 9,67 / 4,48 | 204 / 3 | 118 985 | 138 |

На всех четырёх уровнях неожиданных ошибок и тайм-аутов не было. При переходе с 50 на 100 слотов throughput вышел на плато, а задержка выросла. Этот результат относится к локальному стенду с синтетическим агентом.

За 45 секунд soak с 20 слотами обработано 12 955 сессий, в том числе 554 управляемые отмены и 407 инъекций сбоев загрузки. Зарегистрировано 27 707 heartbeat и 0 неожиданных исходов. Heap составил 3,69 МиБ до прогона, 6,37 МиБ на пике и 4,01 МиБ после GC. Число горутин вернулось с пиковых 44 к исходным 3, журналы удалены. Последние операции завершились за 55 мс после прекращения подачи нагрузки.

CPU profile показал, что на этой Windows-системе около 90% выборки приходится на `runtime.cgocall`, через который проходят системные вызовы. В профиле аллокаций видны буферы NDJSON, разбор и валидация JSON. Block profile в основном показывает штатное ожидание монитора lease. Mutex profile преимущественно содержит блокировки Go runtime. Профили CPU, heap/allocs, block и mutex собраны на бенчмарках, goroutine profile снят с запущенного бинарника.

Буферы NDJSON уменьшены с 16 до 4 КиБ. Для журнала с секретом повторный JSON-разбор теперь нужен при возможном совпадении или наличии escape-последовательности. На тех же наборах бенчмарков общий объём аллокаций сократился с 175 до 142 кБ на одну сессию и со 187 до 155 кБ на сессию при 100 слотах. Ускорение из двух прогонов с разной нагрузкой хоста не выводилось.

Повторный замер без задержки агента дал 137 мкс и 907 аллокаций на валидацию frozen plan, 9,7 мкс и 20 аллокаций на событие NDJSON с публикацией в тестовый sink и 5,5 мкс и 42 аллокации на валидацию маршрута CRM. Метрики реального Runner отдельно учитывают время до claim, длительность ответа агента и длительность сессии.

## Сквозная приёмка

После публикации недостающих контрактов поднимите Platform с validators, Environment с финализатором и Intelligence с симулятором и evaluator. Запустите Runner и агента, поддерживающего согласованный wire v1. В Platform опубликуйте текстовый сценарий с первым POST `/api/v1/orders` под сбоем `timeout_after_save`. Запустите один attempt и проверьте наблюдаемую последовательность.

1. Platform создаёт задание, Runner берёт lease и появляется `session.started.v1`.
2. Intelligence задаёт запрос, агент вызывает `create_order`, а Runner передаёт ему исходный HTTP 504.
3. Environment регистрирует согласованные snapshot и audit. Проверяется количество заявок и история повторов агента.
4. Runner публикует `session.executed.v1` после регистрации manifest. Platform создаёт назначение на оценку.
5. Intelligence получает артефакты, публикует результат оценки, Platform показывает его в отчёте.

Полная цепочка заблокирована недостающими API, перечисленными в [integration.md](integration.md). Тестовые интеграции, описанные выше, проверяют отдельные границы.
 -fuzztime 3s -parallel 4
go test ./internal/transport -run '^## Настоящий Environment и снимок контракта Platform

Поднимите закреплённый Environment на локальном порту и укажите его адрес. В PowerShell:

```powershell
$env:POLIGON_ENVIRONMENT_TEST_URL = "http://127.0.0.1:18081"
$env:POLIGON_PLATFORM_TEST_DIR = "C:/checkouts/poligon-platform/platform"
go test -count=1 ./...
```

`TestRealEnvironmentTimeoutAfterSave` создаёт отдельную CRM-попытку, получает 504 от настоящего Environment и проверяет состояние через `list_orders`. Явный повтор с тем же ключом возвращает существующую заявку. Следующий attempt получает изолированную CRM.

## Настоящая Platform и MinIO

Стенд требует закреплённый checkout Platform, отдельный тестовый PostgreSQL с правом создавать базы и отдельный тестовый MinIO. В fixture Platform используются test-only validators. Overlay добавляет Go-тест в процесс сборки Platform и позволяет оставить исходники checkout на месте. Probe собирается из исходников Runner и вызывает настоящий Control по TCP. Сначала проверяются claim, heartbeat, ref и JCS SHA-256. Затем подписанные PUT отправляются в MinIO, а Platform проверяет фактические байты, регистрирует и разрешает ссылку.

```powershell
$runnerDir = (Get-Location).Path
$platformDir = "C:/checkouts/poligon-platform/platform"
$probeDir = Join-Path $env:TEMP ("poligon-runner-probe-" + [guid]::NewGuid())
New-Item -ItemType Directory $probeDir | Out-Null
$env:POLIGON_RUNNER_PROBE_BIN = Join-Path $probeDir "runner-probe.exe"
go test -c -o $env:POLIGON_RUNNER_PROBE_BIN ./internal/platform

$replace = @{}
$replace[(Join-Path $platformDir "internal/platform/poligon_runner_probe_test.go")] = Join-Path $runnerDir "tests/platformfixture/runner_probe.go.txt"
$overlay = Join-Path $probeDir "overlay.json"
@{ Replace = $replace } | ConvertTo-Json -Compress | Set-Content -LiteralPath $overlay -Encoding utf8

$env:TEST_DATABASE_URL = "postgres://poligon_test@127.0.0.1:15432/postgres?sslmode=disable"
$env:POLIGON_MINIO_TEST_ENDPOINT = "127.0.0.1:19000"
# Задайте POLIGON_MINIO_TEST_ACCESS_KEY и POLIGON_MINIO_TEST_SECRET_KEY для тестового экземпляра.
Push-Location $platformDir
go test -overlay $overlay -v ./internal/platform -run '^TestPoligonRunnerLiveProbe$' -count=1
Pop-Location
```

## Замеры

```powershell
$env:GOMAXPROCS = "4"
$env:POLIGON_RUNNER_LOAD = "3s"
$env:POLIGON_RUNNER_SOAK = "45s"
go test -v ./internal/execution -run '^TestLoadSessions$' -count=1
go test -run '^$' -bench . -benchmem -benchtime 350ms ./internal/execution ./internal/transport
```

Нагрузочный тест использует управляемую задержку агента 40 мс, отдельный scope на слот и тестовые Control, Intelligence, CRM и object storage. В soak есть управляемые отмены и сбои загрузки с восстановлением в следующей сессии. Он проверяет, что после остановки нет активных сессий, оставшихся журналов или устойчивого прироста горутин. Бенчмарки без задержки агента измеряют накладные расходы Runner отдельно. Для pprof доступны `/debug/pprof/profile`, `heap`, `allocs`, `goroutine`, `block` и `mutex` при настроенной диагностической аутентификации.

Фактический прогон на Intel Core Ultra 5 125H с `GOMAXPROCS=4`. Память в таблице — Go heap in use. Числа не включают потребление памяти внешними сервисами и не представляют RSS процесса.

| Слотов | Сессий/с | p50 / p95 / p99, мс | CPU, с | Heap пик / после GC, МиБ | Горутин пик / после | Байт аллокаций на сессию | Drain, мс |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 22,5 | 43,5 / 45,2 / 49,3 | 0,25 | 3,94 / 2,69 | 6 / 3 | 115 506 | 28 |
| 10 | 185,2 | 53,7 / 63,5 / 69,3 | 3,96 | 4,46 / 2,97 | 24 / 3 | 114 081 | 45 |
| 50 | 395,1 | 129,4 / 158,5 / 169,1 | 8,59 | 6,81 / 3,56 | 104 / 3 | 115 722 | 108 |
| 100 | 387,4 | 253,2 / 306,9 / 329,1 | 8,55 | 9,67 / 4,48 | 204 / 3 | 118 985 | 138 |

На всех четырёх уровнях неожиданных ошибок и тайм-аутов не было. При переходе с 50 на 100 слотов throughput вышел на плато, а задержка выросла. Этот результат относится к локальному стенду с синтетическим агентом.

За 45 секунд soak с 20 слотами обработано 12 955 сессий, в том числе 554 управляемые отмены и 407 инъекций сбоев загрузки. Зарегистрировано 27 707 heartbeat и 0 неожиданных исходов. Heap составил 3,69 МиБ до прогона, 6,37 МиБ на пике и 4,01 МиБ после GC. Число горутин вернулось с пиковых 44 к исходным 3, журналы удалены. Последние операции завершились за 55 мс после прекращения подачи нагрузки.

CPU profile показал, что на этой Windows-системе около 90% выборки приходится на `runtime.cgocall`, через который проходят системные вызовы. В профиле аллокаций видны буферы NDJSON, разбор и валидация JSON. Block profile в основном показывает штатное ожидание монитора lease. Mutex profile преимущественно содержит блокировки Go runtime. Профили CPU, heap/allocs, block и mutex собраны на бенчмарках, goroutine profile снят с запущенного бинарника.

Буферы NDJSON уменьшены с 16 до 4 КиБ. Для журнала с секретом повторный JSON-разбор теперь нужен при возможном совпадении или наличии escape-последовательности. На тех же наборах бенчмарков общий объём аллокаций сократился с 175 до 142 кБ на одну сессию и со 187 до 155 кБ на сессию при 100 слотах. Ускорение из двух прогонов с разной нагрузкой хоста не выводилось.

Повторный замер без задержки агента дал 137 мкс и 907 аллокаций на валидацию frozen plan, 9,7 мкс и 20 аллокаций на событие NDJSON с публикацией в тестовый sink и 5,5 мкс и 42 аллокации на валидацию маршрута CRM. Метрики реального Runner отдельно учитывают время до claim, длительность ответа агента и длительность сессии.

## Сквозная приёмка

После публикации недостающих контрактов поднимите Platform с validators, Environment с финализатором и Intelligence с симулятором и evaluator. Запустите Runner и агента, поддерживающего согласованный wire v1. В Platform опубликуйте текстовый сценарий с первым POST `/api/v1/orders` под сбоем `timeout_after_save`. Запустите один attempt и проверьте наблюдаемую последовательность.

1. Platform создаёт задание, Runner берёт lease и появляется `session.started.v1`.
2. Intelligence задаёт запрос, агент вызывает `create_order`, а Runner передаёт ему исходный HTTP 504.
3. Environment регистрирует согласованные snapshot и audit. Проверяется количество заявок и история повторов агента.
4. Runner публикует `session.executed.v1` после регистрации manifest. Platform создаёт назначение на оценку.
5. Intelligence получает артефакты, публикует результат оценки, Platform показывает его в отчёте.

Полная цепочка заблокирована недостающими API, перечисленными в [integration.md](integration.md). Тестовые интеграции, описанные выше, проверяют отдельные границы.
 -fuzz '^FuzzToolRoute## Настоящий Environment и снимок контракта Platform

Поднимите закреплённый Environment на локальном порту и укажите его адрес. В PowerShell:

```powershell
$env:POLIGON_ENVIRONMENT_TEST_URL = "http://127.0.0.1:18081"
$env:POLIGON_PLATFORM_TEST_DIR = "C:/checkouts/poligon-platform/platform"
go test -count=1 ./...
```

`TestRealEnvironmentTimeoutAfterSave` создаёт отдельную CRM-попытку, получает 504 от настоящего Environment и проверяет состояние через `list_orders`. Явный повтор с тем же ключом возвращает существующую заявку. Следующий attempt получает изолированную CRM.

## Настоящая Platform и MinIO

Стенд требует закреплённый checkout Platform, отдельный тестовый PostgreSQL с правом создавать базы и отдельный тестовый MinIO. В fixture Platform используются test-only validators. Overlay добавляет Go-тест в процесс сборки Platform и позволяет оставить исходники checkout на месте. Probe собирается из исходников Runner и вызывает настоящий Control по TCP. Сначала проверяются claim, heartbeat, ref и JCS SHA-256. Затем подписанные PUT отправляются в MinIO, а Platform проверяет фактические байты, регистрирует и разрешает ссылку.

```powershell
$runnerDir = (Get-Location).Path
$platformDir = "C:/checkouts/poligon-platform/platform"
$probeDir = Join-Path $env:TEMP ("poligon-runner-probe-" + [guid]::NewGuid())
New-Item -ItemType Directory $probeDir | Out-Null
$env:POLIGON_RUNNER_PROBE_BIN = Join-Path $probeDir "runner-probe.exe"
go test -c -o $env:POLIGON_RUNNER_PROBE_BIN ./internal/platform

$replace = @{}
$replace[(Join-Path $platformDir "internal/platform/poligon_runner_probe_test.go")] = Join-Path $runnerDir "tests/platformfixture/runner_probe.go.txt"
$overlay = Join-Path $probeDir "overlay.json"
@{ Replace = $replace } | ConvertTo-Json -Compress | Set-Content -LiteralPath $overlay -Encoding utf8

$env:TEST_DATABASE_URL = "postgres://poligon_test@127.0.0.1:15432/postgres?sslmode=disable"
$env:POLIGON_MINIO_TEST_ENDPOINT = "127.0.0.1:19000"
# Задайте POLIGON_MINIO_TEST_ACCESS_KEY и POLIGON_MINIO_TEST_SECRET_KEY для тестового экземпляра.
Push-Location $platformDir
go test -overlay $overlay -v ./internal/platform -run '^TestPoligonRunnerLiveProbe$' -count=1
Pop-Location
```

## Замеры

```powershell
$env:GOMAXPROCS = "4"
$env:POLIGON_RUNNER_LOAD = "3s"
$env:POLIGON_RUNNER_SOAK = "45s"
go test -v ./internal/execution -run '^TestLoadSessions$' -count=1
go test -run '^$' -bench . -benchmem -benchtime 350ms ./internal/execution ./internal/transport
```

Нагрузочный тест использует управляемую задержку агента 40 мс, отдельный scope на слот и тестовые Control, Intelligence, CRM и object storage. В soak есть управляемые отмены и сбои загрузки с восстановлением в следующей сессии. Он проверяет, что после остановки нет активных сессий, оставшихся журналов или устойчивого прироста горутин. Бенчмарки без задержки агента измеряют накладные расходы Runner отдельно. Для pprof доступны `/debug/pprof/profile`, `heap`, `allocs`, `goroutine`, `block` и `mutex` при настроенной диагностической аутентификации.

Фактический прогон на Intel Core Ultra 5 125H с `GOMAXPROCS=4`. Память в таблице — Go heap in use. Числа не включают потребление памяти внешними сервисами и не представляют RSS процесса.

| Слотов | Сессий/с | p50 / p95 / p99, мс | CPU, с | Heap пик / после GC, МиБ | Горутин пик / после | Байт аллокаций на сессию | Drain, мс |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 22,5 | 43,5 / 45,2 / 49,3 | 0,25 | 3,94 / 2,69 | 6 / 3 | 115 506 | 28 |
| 10 | 185,2 | 53,7 / 63,5 / 69,3 | 3,96 | 4,46 / 2,97 | 24 / 3 | 114 081 | 45 |
| 50 | 395,1 | 129,4 / 158,5 / 169,1 | 8,59 | 6,81 / 3,56 | 104 / 3 | 115 722 | 108 |
| 100 | 387,4 | 253,2 / 306,9 / 329,1 | 8,55 | 9,67 / 4,48 | 204 / 3 | 118 985 | 138 |

На всех четырёх уровнях неожиданных ошибок и тайм-аутов не было. При переходе с 50 на 100 слотов throughput вышел на плато, а задержка выросла. Этот результат относится к локальному стенду с синтетическим агентом.

За 45 секунд soak с 20 слотами обработано 12 955 сессий, в том числе 554 управляемые отмены и 407 инъекций сбоев загрузки. Зарегистрировано 27 707 heartbeat и 0 неожиданных исходов. Heap составил 3,69 МиБ до прогона, 6,37 МиБ на пике и 4,01 МиБ после GC. Число горутин вернулось с пиковых 44 к исходным 3, журналы удалены. Последние операции завершились за 55 мс после прекращения подачи нагрузки.

CPU profile показал, что на этой Windows-системе около 90% выборки приходится на `runtime.cgocall`, через который проходят системные вызовы. В профиле аллокаций видны буферы NDJSON, разбор и валидация JSON. Block profile в основном показывает штатное ожидание монитора lease. Mutex profile преимущественно содержит блокировки Go runtime. Профили CPU, heap/allocs, block и mutex собраны на бенчмарках, goroutine profile снят с запущенного бинарника.

Буферы NDJSON уменьшены с 16 до 4 КиБ. Для журнала с секретом повторный JSON-разбор теперь нужен при возможном совпадении или наличии escape-последовательности. На тех же наборах бенчмарков общий объём аллокаций сократился с 175 до 142 кБ на одну сессию и со 187 до 155 кБ на сессию при 100 слотах. Ускорение из двух прогонов с разной нагрузкой хоста не выводилось.

Повторный замер без задержки агента дал 137 мкс и 907 аллокаций на валидацию frozen plan, 9,7 мкс и 20 аллокаций на событие NDJSON с публикацией в тестовый sink и 5,5 мкс и 42 аллокации на валидацию маршрута CRM. Метрики реального Runner отдельно учитывают время до claim, длительность ответа агента и длительность сессии.

## Сквозная приёмка

После публикации недостающих контрактов поднимите Platform с validators, Environment с финализатором и Intelligence с симулятором и evaluator. Запустите Runner и агента, поддерживающего согласованный wire v1. В Platform опубликуйте текстовый сценарий с первым POST `/api/v1/orders` под сбоем `timeout_after_save`. Запустите один attempt и проверьте наблюдаемую последовательность.

1. Platform создаёт задание, Runner берёт lease и появляется `session.started.v1`.
2. Intelligence задаёт запрос, агент вызывает `create_order`, а Runner передаёт ему исходный HTTP 504.
3. Environment регистрирует согласованные snapshot и audit. Проверяется количество заявок и история повторов агента.
4. Runner публикует `session.executed.v1` после регистрации manifest. Platform создаёт назначение на оценку.
5. Intelligence получает артефакты, публикует результат оценки, Platform показывает его в отчёте.

Полная цепочка заблокирована недостающими API, перечисленными в [integration.md](integration.md). Тестовые интеграции, описанные выше, проверяют отдельные границы.
 -fuzztime 3s -parallel 4
```

## Настоящий Environment и снимок контракта Platform

Поднимите закреплённый Environment на локальном порту и укажите его адрес. В PowerShell:

```powershell
$env:POLIGON_ENVIRONMENT_TEST_URL = "http://127.0.0.1:18081"
$env:POLIGON_PLATFORM_TEST_DIR = "C:/checkouts/poligon-platform/platform"
go test -count=1 ./...
```

`TestRealEnvironmentTimeoutAfterSave` создаёт отдельную CRM-попытку, получает 504 от настоящего Environment и проверяет состояние через `list_orders`. Явный повтор с тем же ключом возвращает существующую заявку. Следующий attempt получает изолированную CRM.

## Настоящая Platform и MinIO

Стенд требует закреплённый checkout Platform, отдельный тестовый PostgreSQL с правом создавать базы и отдельный тестовый MinIO. В fixture Platform используются test-only validators. Overlay добавляет Go-тест в процесс сборки Platform и позволяет оставить исходники checkout на месте. Probe собирается из исходников Runner и вызывает настоящий Control по TCP. Сначала проверяются claim, heartbeat, ref и JCS SHA-256. Затем подписанные PUT отправляются в MinIO, а Platform проверяет фактические байты, регистрирует и разрешает ссылку.

```powershell
$runnerDir = (Get-Location).Path
$platformDir = "C:/checkouts/poligon-platform/platform"
$probeDir = Join-Path $env:TEMP ("poligon-runner-probe-" + [guid]::NewGuid())
New-Item -ItemType Directory $probeDir | Out-Null
$env:POLIGON_RUNNER_PROBE_BIN = Join-Path $probeDir "runner-probe.exe"
go test -c -o $env:POLIGON_RUNNER_PROBE_BIN ./internal/platform

$replace = @{}
$replace[(Join-Path $platformDir "internal/platform/poligon_runner_probe_test.go")] = Join-Path $runnerDir "tests/platformfixture/runner_probe.go.txt"
$overlay = Join-Path $probeDir "overlay.json"
@{ Replace = $replace } | ConvertTo-Json -Compress | Set-Content -LiteralPath $overlay -Encoding utf8

$env:TEST_DATABASE_URL = "postgres://poligon_test@127.0.0.1:15432/postgres?sslmode=disable"
$env:POLIGON_MINIO_TEST_ENDPOINT = "127.0.0.1:19000"
# Задайте POLIGON_MINIO_TEST_ACCESS_KEY и POLIGON_MINIO_TEST_SECRET_KEY для тестового экземпляра.
Push-Location $platformDir
go test -overlay $overlay -v ./internal/platform -run '^TestPoligonRunnerLiveProbe$' -count=1
Pop-Location
```

## Замеры

```powershell
$env:GOMAXPROCS = "4"
$env:POLIGON_RUNNER_LOAD = "3s"
$env:POLIGON_RUNNER_SOAK = "45s"
go test -v ./internal/execution -run '^TestLoadSessions$' -count=1
go test -run '^$' -bench . -benchmem -benchtime 350ms ./internal/execution ./internal/transport
```

Нагрузочный тест использует управляемую задержку агента 40 мс, отдельный scope на слот и тестовые Control, Intelligence, CRM и object storage. В soak есть управляемые отмены и сбои загрузки с восстановлением в следующей сессии. Он проверяет, что после остановки нет активных сессий, оставшихся журналов или устойчивого прироста горутин. Бенчмарки без задержки агента измеряют накладные расходы Runner отдельно. Для pprof доступны `/debug/pprof/profile`, `heap`, `allocs`, `goroutine`, `block` и `mutex` при настроенной диагностической аутентификации.

Фактический прогон на Intel Core Ultra 5 125H с `GOMAXPROCS=4`. Память в таблице — Go heap in use. Числа не включают потребление памяти внешними сервисами и не представляют RSS процесса.

| Слотов | Сессий/с | p50 / p95 / p99, мс | CPU, с | Heap пик / после GC, МиБ | Горутин пик / после | Байт аллокаций на сессию | Drain, мс |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 22,5 | 43,5 / 45,2 / 49,3 | 0,25 | 3,94 / 2,69 | 6 / 3 | 115 506 | 28 |
| 10 | 185,2 | 53,7 / 63,5 / 69,3 | 3,96 | 4,46 / 2,97 | 24 / 3 | 114 081 | 45 |
| 50 | 395,1 | 129,4 / 158,5 / 169,1 | 8,59 | 6,81 / 3,56 | 104 / 3 | 115 722 | 108 |
| 100 | 387,4 | 253,2 / 306,9 / 329,1 | 8,55 | 9,67 / 4,48 | 204 / 3 | 118 985 | 138 |

На всех четырёх уровнях неожиданных ошибок и тайм-аутов не было. При переходе с 50 на 100 слотов throughput вышел на плато, а задержка выросла. Этот результат относится к локальному стенду с синтетическим агентом.

За 45 секунд soak с 20 слотами обработано 12 955 сессий, в том числе 554 управляемые отмены и 407 инъекций сбоев загрузки. Зарегистрировано 27 707 heartbeat и 0 неожиданных исходов. Heap составил 3,69 МиБ до прогона, 6,37 МиБ на пике и 4,01 МиБ после GC. Число горутин вернулось с пиковых 44 к исходным 3, журналы удалены. Последние операции завершились за 55 мс после прекращения подачи нагрузки.

CPU profile показал, что на этой Windows-системе около 90% выборки приходится на `runtime.cgocall`, через который проходят системные вызовы. В профиле аллокаций видны буферы NDJSON, разбор и валидация JSON. Block profile в основном показывает штатное ожидание монитора lease. Mutex profile преимущественно содержит блокировки Go runtime. Профили CPU, heap/allocs, block и mutex собраны на бенчмарках, goroutine profile снят с запущенного бинарника.

Буферы NDJSON уменьшены с 16 до 4 КиБ. Для журнала с секретом повторный JSON-разбор теперь нужен при возможном совпадении или наличии escape-последовательности. На тех же наборах бенчмарков общий объём аллокаций сократился с 175 до 142 кБ на одну сессию и со 187 до 155 кБ на сессию при 100 слотах. Ускорение из двух прогонов с разной нагрузкой хоста не выводилось.

Повторный замер без задержки агента дал 137 мкс и 907 аллокаций на валидацию frozen plan, 9,7 мкс и 20 аллокаций на событие NDJSON с публикацией в тестовый sink и 5,5 мкс и 42 аллокации на валидацию маршрута CRM. Метрики реального Runner отдельно учитывают время до claim, длительность ответа агента и длительность сессии.

## Сквозная приёмка

После публикации недостающих контрактов поднимите Platform с validators, Environment с финализатором и Intelligence с симулятором и evaluator. Запустите Runner и агента, поддерживающего согласованный wire v1. В Platform опубликуйте текстовый сценарий с первым POST `/api/v1/orders` под сбоем `timeout_after_save`. Запустите один attempt и проверьте наблюдаемую последовательность.

1. Platform создаёт задание, Runner берёт lease и появляется `session.started.v1`.
2. Intelligence задаёт запрос, агент вызывает `create_order`, а Runner передаёт ему исходный HTTP 504.
3. Environment регистрирует согласованные snapshot и audit. Проверяется количество заявок и история повторов агента.
4. Runner публикует `session.executed.v1` после регистрации manifest. Platform создаёт назначение на оценку.
5. Intelligence получает артефакты, публикует результат оценки, Platform показывает его в отчёте.

Полная цепочка заблокирована недостающими API, перечисленными в [integration.md](integration.md). Тестовые интеграции, описанные выше, проверяют отдельные границы.
