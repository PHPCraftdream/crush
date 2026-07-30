# Повторное ревью доработок и проекта Crush

Дата: 2026-07-30
Проверенный HEAD: `054d23d6` (`fix(test): apply second-round @oh review findings (A-H)`)
База предыдущего ревью: `2b8d0d3b`
Диапазон последних 100 коммитов: `e4ee2385..054d23d6`

## Краткий вывод

Сделанный пакет исправлений заметно улучшил проект. Исправлены наиболее
очевидные потери данных при конкурентных обновлениях сессий, неограниченный
replay WebSocket, опасная замена исполняемого файла, чтение HTTP-body целиком,
ошибочный подсчёт завершённых background jobs и ряд Windows/grep-проблем.

При этом проект ещё нельзя считать полностью защищённым для режима с
несколькими процессами, большим числом параллельных агентов или удалённым web
доступом. Наиболее важные оставшиеся риски:

1. sub-agent не берёт межпроцессный lock своей собственной сессии;
2. reload конфигурации держит глобальный mutex во время потенциально
   пятиминутных shell-resolve операций;
3. подстановка `CRUSH_*` временно меняет process-global environment и
   некорректно восстанавливает ранее отсутствовавшие переменные;
4. permission mutex удерживается всё время ожидания ответа пользователя;
5. WebSocket-команды, live-очереди и direct replies ограничены только числом,
   но не суммарными байтами; обработчики запускаются без лимита;
6. итоговые token counters зависят от гонки между генерацией title и основным
   LLM turn;
7. полный тестовый прогон на Windows падает в `cliprovider` из-за выбора WSL
   `bash.exe` и несовместимого преобразования путей.

Классического подтверждённого взаимного ABBA-deadlock в просмотренных путях не
найдено. Однако есть несколько эквивалентных для пользователя зависаний:
глобальная очередь permissions, долгий config mutex, единственное SQLite
соединение и неограниченные очереди/goroutines web-сервера.

Рекомендация: сначала закрыть пункты P1 ниже, после этого заняться архитектурой
config snapshot и SQLite. Для обычной single-user работы на localhost проект
уже существенно надёжнее, но для активного multi-agent/multi-process режима
hardening ещё не завершён.

## Объём ревью

- Просмотрены последние 100 коммитов: 171 файл, `+22369/-1766`.
- Отдельно просмотрены 20 новых коммитов после предыдущего ревью:
  48 файлов, `+3564/-520`.
- Проведён повторный статический аудит agent/session/config/permission/db,
  shell, deploy, pubsub, HTTP logging и web server.
- Проверены незакоммиченные изменения prompt/документации.
- Выполнен `git diff --check` — ошибок whitespace нет.
- Запущены Go и npm тесты; результаты приведены ниже.
- Исходный код в рамках ревью не изменялся. Добавлен только этот отчёт.

## Что исправлено хорошо

### 1. Обновления сессий стали узкими

Удалено общее сохранение всей строки сессии, вместо него используются
column-scoped операции (`SetUsage`, `SetTodos`, rename, cost). Это устраняет
типичный lost update, когда usage перезаписывал title/todos и наоборот.

Файлы: `internal/session/session.go:529`, `internal/session/session.go:560`.

### 2. Fork сессий теперь транзакционный

И service, и CLI выполняют создание сессии и копирование сообщений в
транзакции и больше не оставляют видимый полупустой fork при ошибке.

Файлы: `internal/session/session.go:318`,
`internal/cmd/sessions_fork.go:121`.

### 3. WebSocket replay действительно ограничен

Replay стал кольцевым буфером O(1), ограничен количеством, общим объёмом и
размером одного события. Это закрывает прежний очевидный рост памяти именно
для replay history.

Файл: `internal/server/hub.go:44`.

### 4. HTTP debug transport больше не буферизует весь ответ

Response body теперь проксируется потоково, а preview ограничен 16 KiB.
Повтор небезопасного POST также не выполняется без idempotency key.

Файл: `internal/log/http.go:17`.

### 5. Deploy получил rollback

Временные файлы уникальны для процесса/запуска, а после неудачного второго
rename старый бинарник восстанавливается. Это значительно лучше прежнего
состояния, где destination мог исчезнуть.

Файлы: `deploy.go:352`, `internal/deploy/deploy.go:72`.

### 6. Остальные закрытые замечания

- config read-modify-write защищён межпроцессным sidecar lock;
- background limit считает только активные задачи;
- grep проверяет cancellation внутри очень длинной строки;
- transcript упорядочен по `created_at, rowid`;
- WSL launcher исключён из shebang resolution;
- повторный Grant/Deny больше не блокируется на отправке в уже заполненный
  response channel.

## P1 — исправить в первую очередь

### P1.1. Sub-agent работает без lock своей сессии

В `Agent.Run` межпроцессный lock берётся только при `!a.isSubAgent`:

`internal/agent/agent.go:484-524`.

Комментарий предполагает, что lock уже держит parent. Но parent держит lock
родительского `sessionID`, а sub-agent пишет в отдельную дочернюю сессию. Пока
sub-agent стримит, второй процесс может открыть именно child session, успешно
взять её lock и начать параллельно писать туда же.

Последствия:

- смешивание stream/checkpoint сообщений;
- неконсистентные usage/title;
- duplicate или неожиданно перезаписанные данные;
- поведение зависит от порядка DB операций.

Что сделать:

- брать session lock для каждого отдельного `sessionID`, включая sub-agents;
- пропускать lock только для явно доказанного reentrant вызова той же сессии,
  передавая ownership token/handle;
- добавить cross-process тест: активный sub-agent блокирует попытку открыть
  его child session вторым процессом.

### P1.2. Config reload держит глобальный mutex во время shell-команд

`ReloadFromDisk` берёт `publishMu` до полной загрузки и отпускает после
`configureProviders`:

- `internal/config/store.go:1304-1310`;
- `internal/config/store.go:1332-1385`.

При этом один `ResolveValue` имеет timeout 5 минут:

`internal/config/resolve.go:12-14`, `internal/config/resolve.go:97`.

Несколько provider keys/headers разрешаются последовательно. Одна зависшая
shell-подстановка способна на минуты заблокировать все config mutations и
следующие reload. Это не data race, но глобальный lock convoy.

Что сделать:

- сериализовать построение candidate generation отдельным `reloadMu`;
- читать базовое поколение и полностью строить/валидировать candidate без
  `publishMu`;
- под коротким `publishMu` выполнить generation check/CAS, rebase runtime
  overrides и один atomic publish;
- передавать caller context в resolver и снизить отдельный timeout;
- добавить тест: зависший resolver не должен блокировать независимое runtime
  изменение конфигурации на минуты.

### P1.3. `PushPopCrushEnv` небезопасен для процесса

`PushPopCrushEnv` копирует `CRUSH_X` в process-global `X`, вызывает resolver,
затем восстанавливает значение через `os.Setenv`:

`internal/config/load.go:189-220`.

Проблемы:

- `os.Getenv` теряет различие между отсутствующей переменной и пустой;
  отсутствовавшая `X` после restore остаётся установленной как `X=""`;
- два параллельных ConfigStore/reload могут перекрыть environment друг друга;
- сторонняя goroutine в этот момент видит временные секреты/значения;
- resolver запускает дочерние процессы с глобально изменённым environment.

Что сделать:

- не менять `os.Environ`;
- собрать immutable overlay `X=value` из `CRUSH_X` и передавать его resolver и
  дочернему процессу явно;
- временный минимальный fix: process-global mutex плюс `LookupEnv`/`Unsetenv`,
  но это всё равно оставит слишком широкую глобальную критическую секцию.

### P1.4. Permission mutex удерживается во время ответа пользователя

`requestMu` берётся в `Request` и удерживается до выхода из `select`, то есть
всё время, пока пользователь не нажмёт Grant/Deny:

`internal/permission/permission.go:244-349`.

Внутри lock также выполняются:

- auto-approve check;
- SQLite lookup persistent grant;
- публикация событий;
- ожидание UI.

Один забытый dialog блокирует все permission requests процесса, включая
запрос другого агента, который мог быть разрешён автоматически или по
persistent grant. Для пользователя это выглядит как зависание всех агентов.

Что сделать:

- перенести allowlist, auto-approve и persistent lookup до сериализации;
- сериализовать только реально интерактивные prompts;
- лучше использовать явную очередь prompts или mutex per session;
- определить UI-протокол для нескольких ожидающих запросов;
- добавить тест: первый интерактивный запрос ждёт, второй auto-approved запрос
  завершается сразу.

### P1.5. WebSocket остаётся уязвим к memory/goroutine exhaustion

Replay исправлен, но live pipeline всё ещё ограничен только числом элементов:

- client send buffer: 512 `[]byte` — `internal/server/hub.go:17`;
- hub broadcast buffer: 1024 `[]byte` — `internal/server/hub.go:121-130`;
- каждая входящая команда запускается в новой goroutine без semaphore —
  `internal/server/handlers.go:29-144`;
- direct reply при заполнении буфера молча отбрасывается —
  `internal/server/hub.go:199-213`;
- panic recovery вокруг WS command handlers отсутствует, что прямо отмечено
  в комментарии — `internal/server/server.go:140-153`.

Большое событие может содержать transcript/tool output. Поэтому count bound не
является memory bound: сотни больших snapshots могут занять сотни мегабайт.
Потерянный request reply оставляет client ждать ответ, который уже никогда не
придёт.

Что сделать:

- общий byte budget для broadcast и каждого client;
- coalescing для state-событий и disconnect медленного клиента;
- отдельный приоритетный канал для request/reply/control;
- semaphore/rate limit на команды одного клиента;
- `recover` на границе каждой command goroutine;
- stress test с большими событиями и проверкой верхней границы памяти.

### P1.6. Usage зависит от порядка завершения title goroutine

Title запускается параллельно основному turn:

`internal/agent/agent.go:578-595`.

Title path выполняет `UpdateTitleAndUsage`, который прибавляет tokens:

- `internal/agent/agent.go:2583`;
- `internal/db/sql/sessions.sql:91-99`.

Основной turn выполняет `SetUsage`, который перезаписывает counters snapshot:

`internal/agent/agent.go:1403-1410`.

Если title завершился первым, следующий `SetUsage` стирает его tokens. Если
title завершился последним, tokens остаются. Итоговая статистика
недетерминирована, хотя SQL операции по отдельности атомарны.

Что сделать:

- сначала определить семантику: counters — cumulative usage всех model calls
  или snapshot только conversation;
- если title не входит в counters, title path должен делать Rename +
  IncrementCost без token increment;
- если входит — все вызовы должны использовать единый additive ledger, а не
  смешивать add и overwrite;
- добавить барьерный тест обеих очередностей и многократный race test.

### P1.7. `go test ./...` падает на Windows в `cliprovider`

В текущем окружении первым в PATH найден
`C:\Windows\System32\bash.exe` (WSL launcher), затем Git Bash. Тесты напрямую
задают `Binary: "bash"` и передают `D:/...` или `D:\...` paths. WSL bash эти
пути не понимает.

Падают как минимум:

- `TestStreamWithPartParser` — `provider_test.go:630`;
- `TestStreamWithCodexParser` — `provider_test.go:979`;
- `TestStreamWithGeminiParser` — `provider_test.go:1045`;
- `TestStreamFastExitNoLastLineLoss` — `provider_test.go:1124`;
- `TestStreamWaitBoundedOnGrandchildHoldsStderr` — `provider_test.go:1315`;
- `TestStreamKillUsesTreeKillStillTerminatesChild` — `provider_test.go:1567`.

Это показывает, что недавний fix WSL resolution в shell dispatch не охватывает
`cliprovider`, а тесты зависят от неявного вида `bash` в PATH.

Что сделать:

- создать общий Windows resolver shell binary, который различает WSL launcher
  и Git Bash, и использовать его также в `cliprovider`;
- либо тестам явно находить Git Bash и корректно преобразовывать путь через
  `cygpath`;
- WSL-variant тестировать отдельно с `/mnt/<drive>/...`;
- не считать suite зелёным на основании машины, где Git Bash случайно идёт
  раньше System32.

## P2 — важные архитектурные доработки

### P2.1. Контракт immutable config snapshot нарушается

`Config()` документирован как read-only, но production code мутирует
опубликованные объекты:

- `ConfigStore.SetupAgents` — `internal/config/store.go:144-179`;
- `disableToolsInConfig` — `internal/app/app.go:173-190`;
- provider updates in place — `internal/config/store.go:688-764`,
  `internal/config/store.go:779-783`;
- OAuth refresh меняет map, прочитанную из конкретного snapshot, и может
  записать в уже orphaned generation — `internal/config/store.go:792-868`.

`csync.Map` защищает внутреннюю map от data race, но не восстанавливает
атомарность поколения. Читатель может видеть config generation с provider map,
изменённой “после публикации”, а reload может сделать update невидимым.

Нужен единый copy-on-write API:

- clone config + nested maps/slices;
- применить mutation;
- atomic publish нового поколения;
- не возвращать наружу mutable `*Config`, либо возвращать read-only view/copy.

### P2.2. Одно SQLite соединение — глобальная очередь всего приложения

`internal/db/connect.go:85-90` устанавливает `SetMaxOpenConns(1)` как workaround
для прежнего `SQLITE_NOTADB`.

Это последовательно выполняет все чтения и записи. Долгий fork, recursive CTE,
GC или transcript query блокирует checkpoints всех параллельных sub-agents.
Транзакционный fork дополнительно читает все сообщения и вставляет их по одному.

Предлагаемая последовательность:

1. воспроизвести и локализовать первопричину `SQLITE_NOTADB`;
2. проверить одинаковые pragmas на каждом соединении;
3. отделить сериализованный writer от небольшого read pool;
4. заменить message-copy loop на bulk `INSERT ... SELECT`;
5. добавить benchmark: 10 streaming agents + fork + transcript reads, измерять
   p50/p95/p99 времени DB ожидания.

### P2.3. Grant и Deny могут опубликовать противоречивые outcomes

`GrantPersistent`, `Grant` и `Deny` сначала публикуют notification и только
потом вызывают `pendingRequests.Take`:

`internal/permission/permission.go:133-218`.

При конкурентном повторном ответе только один handler разрешит channel, но
подписчики могут увидеть и granted, и denied. `GrantPersistent` также способен
сохранить grant для уже разрешённого запроса.

Нужно сначала атомарно `Take`; только победитель должен публиковать outcome,
писать persistent grant и очищать active request.

### P2.4. File lock классифицирует любую lock-stage ошибку как contention

`TryAcquireFileLock` оборачивает любую ошибку `tryLockFile` текстом
`file lock contended`, после чего `AcquireFileLockContext` распознаёт
contention через `strings.Contains`:

`internal/session/file_lock.go:65-147`.

Permission error, unsupported filesystem или I/O failure будут повторяться до
timeout вместо немедленного возврата. В OS-specific коде уже есть возможность
различать lock contention; следует использовать typed/sentinel error и
`errors.Is/As`.

### P2.5. Remote web mode передаёт token по plaintext HTTP

WebSocket разрешает любой Origin:

`internal/server/server.go:18-23`.

CLI допускает bind на `0.0.0.0`, но сервер не предоставляет TLS. Cookie/token
передаются по HTTP, cookie не `Secure`, а query-token fallback способен попасть
в URL logs/history.

Для localhost риск ограничен. Для LAN/remote:

- отказывать в non-loopback bind без явного `--insecure-remote`;
- либо поддержать TLS/reverse-proxy trusted mode;
- ограничить Origin;
- убрать query token после browser bootstrap.

### P2.6. Параллельные deploy всё ещё могут смешать версии

Уникальные temp/aside имена устранили collision, но межпроцессного deploy lock
нет. Два deploy разных commits могут чередовать замену нескольких destination,
получив набор бинарников разных версий. Один процесс также может принять
действие второго за failure собственного rollback.

Добавить global/per-destination lock, manifest версии и post-deploy
verification. Если все destination должны соответствовать одной версии,
нужен transaction-like manifest/rollback plan.

### P2.7. OFFSET pagination нестабильна при активных вставках

`ORDER BY created_at, rowid` даёт полный порядок на неизменной таблице, но
`LIMIT/OFFSET` всё равно сдвигает страницы, если child session продолжает
получать сообщения между запросами:

`internal/db/sql/messages.sql:76-87`.

Count и page читаются отдельными запросами. Возможны duplicate/skip между
страницами.

Перейти на keyset cursor `(created_at, rowid)` и при необходимости фиксировать
high-water mark последнего rowid на начало чтения transcript.

### P2.8. Pubsub buffer также ограничен только количеством

Каждый subscriber получает channel на 4096 событий:

`internal/pubsub/broker.go:35-40`.

Message checkpoint может быть полной копией растущего сообщения, поэтому
4096 элементов могут удерживать большой объём памяти ещё до WebSocket hub.
Нужны byte budget и coalescing latest-state событий по session/message ID.

## P3 — технический долг и локальные улучшения

### P3.1. HTTP body debug logging раскрывает содержимое prompts

Preview ограничен 16 KiB, но body всё равно может содержать prompt, tool
output, access key в JSON и пользовательские данные. Header redaction этого не
закрывает.

Сделать body logging отдельным явно небезопасным флагом, по умолчанию
логировать только method/status/length/duration и редактировать известные JSON
поля.

### P3.2. Остались внешние response body без размера

Неограниченный `io.ReadAll` остаётся как минимум здесь:

- `internal/agent/tools/search.go:72`;
- `internal/oauth/copilot/oauth.go:57`;
- `internal/oauth/copilot/oauth.go:167`;
- `internal/update/update.go:117`.

Использовать `io.LimitReader` и возвращать явную ошибку при превышении лимита.

### P3.3. Call tree ограничен по глубине, но не по ширине

Большой fan-out одной вершины всё равно материализует очень много строк.
Ограничить количество children/spawns на session либо добавить общий budget и
query timeout.

### P3.4. NPM cache key не является content hash

`npm/crush/bin/crush.js:67-81` использует metadata (size, mtime, version).
Замена бинарника файлом того же размера с сохранённым mtime оставит stale
cache. Использовать SHA-256/build ID. Текущие npm cache tests проходят, но этот
сценарий не покрыт.

### P3.5. `csync.Map.GetOrSet` не атомарен

`internal/csync/maps.go:80-88` выполняет Get, затем callback, затем Set.
Несколько goroutines могут одновременно вычислить разные значения. Либо
переименовать API, чтобы не обещать атомарность, либо использовать
double-check/singleflight.

### P3.6. Некоторые тесты вызывают `require` из worker goroutine

Новые session tests содержат `require.NoError(t, ...)` внутри goroutines:

- `internal/session/session_test.go:484-493`;
- `internal/session/session_test.go:525-534`.

`testing.T`/`FailNow` нельзя использовать таким образом. Ошибки нужно вернуть
через channel и проверить в test goroutine. Последний commit уже исправляет
аналогичный дефект в другом тесте, но не во всех местах.

### P3.7. `go.sum` содержит tool-induced шум

После запуска linter через `go run` добавлено 183 строки checksums без изменения
`go.mod`. `go mod tidy -diff` показывает большой diff (539 строк вывода).

Не запускать dev tools внутри основного module graph; использовать отдельный
tools module/закреплённый binary. Перед merge выполнить осознанный `go mod
tidy` и проверить, что не удалены действительно необходимые суммы.

### P3.8. CLI и web fork дублируют реализацию

Две отдельные транзакционные реализации уже начали расходиться по деталям.
Вынести общий service API с options (`fork point`, title, message selection),
чтобы фиксы атомарности и производительности не приходилось повторять.

### P3.9. Незакоммиченные prompt-изменения слишком жёсткие

В `internal/agent/templates/coder.md.tpl` появилось общее требование
“Under 4 lines of prose per turn”. Для диагностики, security review и сложного
handoff это может отрезать доказательства и важные оговорки. Также zero-trust
verification фактически отложена до одного финального прохода, из-за чего
ошибки последовательных chunks могут накапливаться.

Лучше:

- “compact by default”, но с явным исключением для diagnosis/review/handoff;
- верифицировать на integration boundaries, а не только один раз в самом
  конце;
- добавить prompt regression scenarios для сложного review и multi-step
  implementation.

## Анализ блокировок

| Область | Текущее поведение | Риск |
|---|---|---|
| Session OS lock | Parent lock есть, child sub-agent lock пропущен | Параллельная запись двух процессов в child session |
| Config `publishMu` | Удерживается во время чтения, provider setup и shell resolve | Глобальная остановка config writes на минуты |
| Config file lock | Sidecar lock вокруг disk RMW, порядок в целом последователен | Хорошее исправление; нужен typed error и метрики ожидания |
| Permission `requestMu` | Удерживается до ответа пользователя | Все permission requests выстраиваются за одним dialog |
| SQLite pool | `MaxOpenConns(1)` | Не deadlock, но глобальный head-of-line blocking |
| WebSocket queues | Count bounded, byte unbounded | Memory pressure, потерянные replies |
| Pubsub | 4096 сообщений на subscriber | Большие snapshots удерживают память |

Подтверждённого цикла вида lock A → lock B / lock B → lock A в изученных
ветках не обнаружено. Основная опасность сейчас — не mutex deadlock, а слишком
широкие critical sections, неограниченное ожидание внешнего события и скрытые
глобальные сериализаторы.

## Результаты проверок

### Успешно

- `git diff --check 2b8d0d3b..HEAD`;
- `node --test npm/crush/bin/crush.cache.test.js`:
  4 секции, все PASS;
- `go test ./internal/config ./internal/session ./internal/permission
  ./internal/server ./internal/log ./internal/deploy -count=1 -timeout=5m`;
- `go test -race ./internal/config ./internal/session ./internal/permission
  ./internal/server -count=1 -timeout=6m`;
- `go vet ./internal/config ./internal/session ./internal/permission
  ./internal/server ./internal/log ./internal/deploy`;
- `internal/agent`, `internal/agent/agentguard`,
  `internal/agent/prompt`, `internal/agent/tools`,
  `internal/agent/tools/mcp`, `internal/app` в общем Go-прогоне прошли.

### Неуспешно

`go test ./... -timeout=12m` не является зелёным: пакет
`internal/agent/cliprovider` падает на шести Windows/bash tests, перечисленных
в P1.7. Причина воспроизводится: `Get-Command bash -All` первым возвращает WSL
launcher из `System32`, а тестовые пути рассчитаны на другой shell. После
зафиксированных падений общий процесс более 12 минут продолжал потреблять CPU
без нового вывода и был остановлен; поэтому это отрицательный, но не полный
прогон всех оставшихся пакетов.

`go mod tidy -diff` возвращает ненулевой status и diff; текущий `go.sum` не
tidy.

`golangci-lint` в окружении не установлен, поэтому независимый полный lint
прогон в рамках этого ревью не выполнен.

## План доработок

### Этап 1 — correctness и liveness, 1–3 дня

1. Брать OS session lock для sub-agent child sessions.
2. Сократить область `requestMu`, исправить Take-before-publish.
3. Убрать process-global env mutation из config resolution.
4. Вынести тяжёлый config reload из-под `publishMu`.
5. Унифицировать Windows shell resolution для shell и cliprovider; сделать
   `go test ./...` зелёным на машине с WSL + Git Bash.
6. Определить и исправить семантику token accounting.

Критерий выхода: новые targeted concurrency tests, зелёный полный Go suite на
Windows и Linux, повторный `-race` на затронутых пакетах.

### Этап 2 — web и storage hardening, 3–7 дней

1. Byte-bounded WebSocket/pubsub, coalescing, priority replies.
2. Per-client command semaphore и panic recovery.
3. Полный immutable/copy-on-write ConfigStore.
4. Typed file-lock errors.
5. Keyset transcript pagination.
6. Deploy lock/version manifest.
7. Безопасная политика non-loopback web bind.

Критерий выхода: stress tests с измеряемым memory ceiling, отсутствие
потерянных replies, стабильный transcript при конкурентной записи.

### Этап 3 — производительность и поддерживаемость

1. Диагностировать `SQLITE_NOTADB`, затем разделить writer/read pool.
2. Bulk fork через `INSERT ... SELECT`, убрать дублирование CLI/web.
3. Добавить DB contention metrics и benchmarks.
4. Ограничить внешние bodies и call-tree width.
5. SHA-256 для npm cache.
6. Очистить `go.sum`, исправить goroutine assertions и prompt regressions.

## Минимальный набор новых тестов

1. Второй процесс не может писать в активную child session sub-agent.
2. Зависший config resolver не блокирует независимый runtime update.
3. Два параллельных reload не меняют process environment.
4. Интерактивный permission request не блокирует auto-approved request.
5. Конкурентные Grant/Deny публикуют ровно один terminal outcome.
6. 1000 больших WS events не превышают заданный memory budget.
7. Direct reply либо доставляется, либо connection закрывается с явной
   ошибкой — молчаливой потери нет.
8. Title и main turn дают одинаковые counters при обеих очередностях.
9. Transcript pagination не теряет строки при concurrent inserts.
10. Два параллельных deploy не создают mixed-version installation.
11. Windows CI с WSL launcher первым в PATH и Git Bash вторым.
12. Benchmark 10 sub-agents + fork + message reads с p95/p99 DB wait.

## Итоговый приоритет

Если выбирать только пять задач на ближайший цикл:

1. sub-agent session lock;
2. config reload/env redesign;
3. permission serialization и terminal outcome;
4. WebSocket byte bounds + command semaphore + reliable replies;
5. deterministic usage accounting и зелёный Windows test suite.

После этих пяти пунктов главный системный риск сместится от correctness к
производительности SQLite, и тогда уже оправдан отдельный storage benchmark и
рефакторинг connection model.
