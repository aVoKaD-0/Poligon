# Проверка реализации

Дата: 30.10.2026. Проверяется только Platform/backend 1. Финальный прогон: 18 тестов верхнего уровня, без ошибок и без пропуска интеграционных тестов.

Проверки запускаются без платных API и без исходников соседних сервисов. PostgreSQL 17.6 и NATS Server 2.11.9 запущены временно на loopback, отдельно от пользовательских данных. Каждый интеграционный DB-тест создаёт и удаляет собственную database. Используется Go race detector.

Пройденные сценарии фиксируются командой `make integration`: idempotent create с восемью одновременными запросами, If-Match, immutable versions/plan, retry после expiry, stale result/inbox dedup, отмена, manual evaluation, CSRF/tenant isolation/last owner, OIDC/PKCE с подписанными токенами и негативными проверками, service identity в gRPC, artifact checksums/immutable staging, executed→evaluation→report, восстановление journal, SSE replay/logout и 20 конкурентных claims.

S3 wire в интеграционных тестах — ограниченный тестовый HTTP server, работающий с реальным MinIO SDK. Он проверяет обработку bytes, staging и registry, но не доказывает совместимость подписей с настоящим MinIO/S3. Для этого требуется Compose smoke.

Конфигурация NATS проверена `nats-server -t`; Compose проверен `docker compose config --quiet`. TypeScript client проверен TypeScript 5.9.3 с `--strict --noEmit --target es2022 --lib es2022,dom`. `go vet` и сборка выполняются отдельно.

Docker daemon в текущей среде отсутствует, поэтому build images и контейнерный smoke не выполнены. Restore на независимый MinIO и production mTLS deployment также не подтверждены. Инструкция drill есть в operations.md; шифрование и невозможность decrypt без исходного master key проверены автоматически.

Полный P0 с багованным/исправленным агентом, фактические 20 одновременных разговоров, голосовые прогоны и качество ML требуют остальных сервисов. Transport fixtures не являются результатом такого эксперимента. Validation RPC wire proposal требует согласования с Environment/Intelligence.

## Замер Platform

Тест `Test20ConcurrentExecutionClaimsAndReadLatency` создаёт два runs по десять сессий, одновременно получает двадцать leases и выполняет 100 последовательных авторизованных REST reads. Он измеряет orchestration/read path при активных назначениях, а не исполнение разговоров и не нагрузку providers. Порог SLA не задан. Последний запуск: Mac17,2, 10 CPU, 16 GiB RAM; Go 1.27.2, Darwin arm64, PostgreSQL 17.6, NATS 2.11.9, race detector включён. Финальный прогон: p50=0.607 мс, p95=0.668 мс, p99=0.952 мс. Предыдущий прогон на том же стенде: p50=0.963 мс, p95=6.057 мс, p99=18.928 мс; кэш и фоновая нагрузка между прогонами не контролировались. Это однократный локальный замер, не SLA и не общий benchmark Полигона.

Собранный standalone binary также проверен настоящими HTTP-запросами на loopback: health, local login/me, создание/чтение проекта, создание/redaction credentials, logout и отзыв сессии. Контейнерный smoke остаётся отдельной непроведённой проверкой.
