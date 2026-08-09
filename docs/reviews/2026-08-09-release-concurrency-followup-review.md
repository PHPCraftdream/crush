# Повторный release-review конкурентности и зависаний сессий

Дата: 2026-08-09

Проверенный HEAD: `33a696c7e8f5bdd370526d024e6a12b2c712bcb9`

Базовый отчёт: `d5bbfeae` (`docs/reviews/2026-08-07-release-concurrency-review.md`)

Основной диапазон: `d5bbfeae..33a696c7` — 10 коммитов, 29 файлов,
примерно `+4468/-134`.

Режим: только статическое чтение кода и истории Git. Тесты, сборка и lint не
запускались по просьбе пользователя.

## Итог

**Вердикт: NO-GO для стабильного релиза.**

Большая часть исправлений полезна и закрывает конкретные окна гонок, но главный
release-инвариант всё ещё не доказан:

> Если запрос принят или сохранён как queued, он либо получает живого runner,
> либо возвращает явную ошибку; teardown никогда не оставляет mailbox навсегда
> busy без исполняющей горутины.

В проверенном коде остаются как минимум два непосредственно достижимых
release-blocker класса:

1. `SessionLock.Release()` снимает OS-lock раньше очистки metadata, но всё ещё
   синхронно ждёт потенциально бесконечный filesystem I/O. Mailbox до возврата
   остаётся `mbOwned` или `mbReleasing`; новый запрос может быть принят в очередь
   без runner.
2. Повторный запуск orphaned/detached work ограничен пятью попытками или одной
   попыткой, после чего работа либо остаётся только в памяти до *чужого будущего*
   `Run()`, либо исчезает. Тесты «execution» сами искусственно вызывают этот
   следующий `Run()`/`startDetachedRun()`, которого production не гарантирует.

Классического нового цикла `mutex A -> mutex B -> mutex A` в изученном диапазоне
не найдено. Однако это не означает отсутствия зависаний: оставшиеся проблемы —
liveness deadlocks/lost wakeups вокруг синхронного I/O, ownership и runner
handoff. Для пользователя они выглядят так же: сессия бесконечно busy, queued
запрос не выполняется или UI показывает противоречивое состояние.

## Что изменилось после прошлого отчёта

| Коммит | Заявленная цель | Статический результат |
|---|---|---|
| `3d8810eb` | единый ownership-exit finalizer | Частично закрыто: error paths теперь handoff-ят очередь, но finalizer не достигается при зависшем `Release()` |
| `37f1820d` | retry/requeue orphaned и detached calls | Не закрыто: retry bounded, очередь недолговечна, некоторые пути не requeue-ят вообще |
| `e91ff0ee` | rerun fail-closed | Закрыто в изученном web path |
| `d1c9ab45` | immutable summary snapshot и cancel-immune cleanup | Cleanup закрыт; snapshot manual summary остаётся несогласованным и не target-session-scoped |
| `334a9dc4` | bounded shutdown | Время возврата ограничено, но resource-ordering и живые writer goroutines остаются небезопасными |
| `26b18e55` | authoritative busy events и summarize drain | Частично: обычный `Run` улучшен, queued-summary transitions всё ещё неверны/неполны |
| `36c9a3b9` | manual compaction через `mbReleasing` | Состояние стало диагностичнее, но сам freeze не устранён |
| `17abf3e5` | watchdog manual compaction и cancellation conformance | Watchdog добавлен; hard execution boundary для всех providers не доказан |
| `7d8072e1` | доказать execution в P0-2 tests | Тесты доказывают выполнение только после внешнего ручного триггера |
| `33a696c7` | changelog/checkpoints | Документация преждевременно заявляет закрытие всего раунда |

## Release blockers

### P0-1. `SessionLock.Release` всё ещё может навсегда заморозить mailbox

`SessionLock.Release()` правильно поменял порядок: сначала `unlockFile` и
`Close`, затем диагностическая очистка. Но `clearHolderMetadataFn(l.Path)` всё
ещё вызывается синхронно внутри `sync.Once.Do`
(`internal/session/lock.go:282-311`). Сама очистка делает `OpenFile`,
`Truncate`, `Seek`, `Sync` и `Remove` без context/timeout; комментарий к коду
прямо признаёт, что она может зависнуть бесконечно
(`internal/session/lock.go:338-365`).

Это сохраняет два freeze path:

- нормальный финальный drain переводит mailbox в `mbReleasing`, вызывает
  release и переводит его в `mbIdle` только после возврата
  (`internal/agent/mailbox.go:496-605`);
- на раннем выходе из `Run` defer с `lk.Release()` исполняется раньше defer с
  `abandonOwnershipWithHandoff` (`internal/agent/agent.go:1266-1268,
  1345-1349`). При зависании release mailbox остаётся `mbOwned`, а единый
  finalizer вообще не запускается;
- manual compaction аналогично ждёт `lk.Release()` между `beginRelease` и
  `finishRelease` (`internal/agent/agent.go:3173-3197`). Watchdog к этому
  моменту уже не помогает: он управляет context provider stream, а не
  filesystem-вызовом без context.

OS-lock при этом уже свободен, но in-process mailbox продолжает считаться busy.
Новый `Run()` видит `mbReleasing`, складывает call в `submitted` и возвращает
`nil`; живого turn loop, который заберёт этот call, уже нет. Это прямой
пользовательский freeze, а не только ухудшение диагностики.

**Что исправить:** отделить correctness-critical `unlock + close` от metadata
cleanup на уровне API. Mailbox должен завершать переход в `mbIdle` сразу после
успешного critical release. Диагностическую очистку либо не выполнять вообще
(stale metadata безопаснее, потому что OS-lock уже является source of truth),
либо запускать как best-effort работу, которая не удерживает ownership и не
участвует в shutdown join.

### P0-2. Принятый detached/orphaned call всё ещё не имеет гарантированного runner

Здесь остаются несколько вариантов одного нарушения.

#### A. Web interrupt может исчезнуть после bounded retry

`coordinator.startDetachedRun` делает пять попыток `currentAgent.Run`
(`internal/agent/coordinator.go:2229-2279`). После исчерпания попыток durable
row восстанавливается только для call с `InjectID` и `ExistingMessageID`
(`internal/agent/coordinator.go:2283-2309`). Обычный web
`InterruptAndSend`, попавший в локально idle session, пока другой процесс держит
OS-lock, не имеет ни того, ни другого. Его prompt ещё не записан в DB. После
пяти попыток функция только логирует ошибку и ранее уже вернула клиенту
`{"status":"queued"}` (`internal/server/handlers.go:377-386`). Prompt потерян.

#### B. Normal-release orphaned path всё ещё делает только одну попытку

`drainOrReleaseMerged` запускает raced-in calls через `restartOrphaned`, а не
`restartOrphanedWithRetry` (`internal/agent/agent.go:946-950`).
`restartOrphaned` вызывает `a.Run` ровно один раз и после проигрыша
межпроцессного OS-lock только пишет log (`internal/agent/agent.go:1034-1043`).
Call не сохраняется и не получает следующего runner.

#### C. Retry exhaustion сохраняет call, но не будит исполнителя

`restartOrphanedWithRetry` после пяти неудач делает только
`mailbox.queue(call)` (`internal/agent/agent.go:1080-1133`). Сам комментарий
признаёт residual: без будущего `Run()` call останется в `submitted` навсегда.
Это сохранность в памяти, но не liveness и не durability.

Регрессионный тест подтверждает именно этот пробел: во второй фазе он сам
освобождает lock и вызывает новый throwaway `Run()`
(`internal/agent/p0_2_regression_test.go:143-157`). Production-события,
гарантирующего такой вызов, нет.

#### D. Восстановленный `pending_injects` также не получает гарантированный pump

После ошибки detached run row создаётся заново с новым ID. Но interrupt ticker
живёт только пока идёт owning turn и завершает работу после первого consumed row
(`internal/agent/coordinator.go:2057-2085`). Если row восстановлена после его
выхода, idle process её не poll-ит. Тест снова вручную вызывает
`startDetachedRun` во второй фазе
(`internal/agent/p0_2_cross_process_test.go:202-211`), а не доказывает
автоматический pickup production ticker-ом.

**Что исправить:** нужен один durable per-session run queue с
`pending -> leased -> acked`, idempotency key и pump, который живёт независимо
от request/turn. До появления такой очереди нельзя отвечать `queued`, пока
payload не записан durable. Как минимальный релизный вариант все detached и
orphaned paths должны использовать один supervisor, который продолжает retry до
acquire/explicit shutdown и умеет возвращать клиенту terminal failure; bounded
retry плюс «может быть когда-нибудь придёт другой Run» недостаточен.

## Высокий приоритет

### P1-1. Manual summary по-прежнему собирается из двух разных snapshots

Coordinator один раз читает `currentAgent.Model()` и вычисляет provider options
(`internal/agent/coordinator.go:2501-2519`). Затем, уже внутри agent,
`runSummarize` отдельно вызывает `resolveTurnConfig(SessionAgentCall{})`
(`internal/agent/agent.go:3163-3164`). Между этими чтениями concurrent
`SetModels` может сменить shared state. Итог: options/provider config от модели A,
а фактическая model/prefix от B.

Кроме того, оба чтения берут process-global last-writer-wins state, а не pinned
models целевой сессии. Сам комментарий в production-коде называет это
«minimal fix» и оставляет target-session resolution на будущий refactor.
Queued summary усугубляет проблему: в `summarizeQueue` сохраняются только
`ProviderOptions`, а модель разрешается позже из текущего shared state.

Inline summary внутри обычного turn исправлен лучше: туда передаются
`largeModel` и `promptPrefix` уже разрешённого turn
(`internal/agent/agent.go:2947,2970`). Поэтому прежний P1-3 закрыт только для
inline path, но не для manual/queued compaction.

**Что исправить:** coordinator должен создать единый immutable
`SummarizeRequest` из target session: model, provider, provider options,
reasoning, prefix и auth identity. Agent не должен повторно читать shared
model state.

### P1-2. Очередь manual summary и web state transitions всё ещё расходятся с ownership

`abandonOwnershipWithHandoff` теперь забирает `summarizeQueue` и запускает
detached `Summarize` (`internal/agent/agent.go:971-1006`). Но успешный manual
summary завершает ownership через plain `mb.abandonOwnership`, затем дренит
только обычные submitted calls (`internal/agent/agent.go:3209-3219`). Если
второй `/compact` был поставлен в очередь во время первого успешного `/compact`,
его `summarizeQueue` никто не забирает. Он может остаться queued навсегда.

Web events тоже не следуют этим переходам:

- при `ErrSummarizeQueued` handler отправляет `agent_busy=false`, хотя исходный
  turn всё ещё владеет session (`internal/server/handlers.go:1257-1262`);
- detached drain не отправляет `summarize_queued=false` и `agent_busy=true`;
- единственный production `summarize_queued=false` находится в explicit cancel
  handler (`internal/server/handlers.go:1289`);
- после запуска detached summary исходный send-handler может успеть увидеть
  короткое `mbIdle` окно и отправить `busy=false`; последующего `busy=true`
  transition нет.

Backend serialization частично защищает DB, но UI получает ложное idle/queued
состояние — именно тот симптом, который пользователи описывают как зависшую
сессию.

**Что исправить:** очередь compaction должна дрениться из каждого terminal
ownership transition, включая успешную compaction, либо повторные compaction
должны явно coalesce-иться с завершением request. `agent_busy` и
`summarize_queued` нужно публиковать из state transitions mailbox/queue, а не из
lifetime отдельных websocket handlers.

### P1-3. Shutdown теперь bounded по времени, но не является безопасным join

`CancelAll` ждёт максимум пять секунд и сообщает `stillBusy`
(`internal/agent/agent.go:4252-4295`). `App.Shutdown` после этого всё равно
запускает cleanup, а через десять секунд возвращается даже при живых cleanup
goroutines (`internal/app/app.go:1804-1868`). Это устраняет бесконечный wait
вызывающего потока, но не выполняет комментарий «agents finish before DB close».

DB cleanup запускает `db.Release` в фоновой горутине и abandons её по timeout
(`internal/app/app.go:172-205`). `db.Release`, в свою очередь, держит глобальный
`poolMu` во время `sql.DB.Close` (`internal/db/connect.go:192-218`). Если close
завис, Shutdown вернётся, но pool mutex останется занят навсегда; живой agent
может одновременно продолжать писать в закрывающуюся DB. Для CLI process exit
это принятый forced-exit компромисс, но для long-lived embedding/server restart
это poisoned process state, а не graceful shutdown.

**Что исправить:** разделить graceful и forced shutdown. Порядок:
`stop accepting -> cancel -> real dispatcher join -> close resources`. Если join
не завершился, CLI может выйти с явным forced status, но library/server path не
должен закрывать DB под writers. Закрытие SQL handles не должно удерживать
глобальный pool mutex: entry следует атомарно перевести в closing/remove state,
а потенциально долгий `Close` выполнить вне mutex.

### P1-4. Watchdog всё ещё не является hard execution boundary

Manual compaction теперь имеет watchdog, но его force action — только
`cancel()`. Если provider/transport игнорирует context, синхронный `Stream`
продолжает блокировать owner. Новый conformance test покрывает
`openaicompat`; commit message прямо фиксирует, что Anthropic, Bedrock, Google,
OpenAI, OpenRouter и Vercel отдельно не проверены. Поэтому утверждение «нет
больше freezes» для всех поддерживаемых providers пока недоказуемо.

Это не требует отдельной горутины вокруг каждого provider вызова без ownership
design: такой timeout лишь бросит writer в фоне. Нужен provider-level hard abort
контракт (HTTP request cancellation/transport close, process-tree kill для CLI)
и одна и та же conformance suite для каждого adapter category.

## Средний приоритет и общий обзор

### P2-1. Очистка lock metadata после unlock повреждает metadata нового owner

После unlock другой process может сразу приобрести тот же lock и записать свой
PID/sidecar. Старый releaser затем выполняет `clearHolderMetadata` и способен
truncate/remove уже metadata нового владельца
(`internal/session/lock.go:282-310,365-390`). На POSIX advisory lock этому не
мешает; на Windows как минимум sidecar остаётся незащищённым.

OS mutual exclusion не ломается, но `sessions locks/why/kill` может потерять PID
реально живого holder. Rescue tooling fail-closed, поэтому результатом будет
невозможность диагностировать/остановить зависший процесс, пока тот не завершится.
Нельзя очищать ownership metadata после потери ownership без generation/token
compare. Самый безопасный простой вариант — оставить stale metadata и всегда
доверять probe OS-lock, что CLI уже делает.

### P2-2. Новые regression tests содержат глобальные test seams под `t.Parallel`

`TestP2_3_ManualCompactionWatchdogCatchesIdleStall` вызывает `t.Parallel`, затем
перезаписывает package-global `testStreamWatchdogTick`
(`internal/agent/p2_3_regression_test.go:190,240-241`). Параллельные watchdog
tests читают эту переменную. Аналогично session regression test с
`t.Parallel` заменяет глобальный `clearHolderMetadataFn`
(`internal/session/p1_2_regression_test.go:35,52-62`), пока другие parallel lock
tests вызывают `Release`.

Это источник data race и order-dependent false pass/failure. Особенно опасно,
что именно эти тесты используются как доказательство release safety. Test
dependencies нужно инжектить в instance/options, либо такие тесты и все readers
глобального seam должны быть сериализованы.

### P2-3. State machine слишком велика и дублирует recovery policy

Критическая логика распределена между `agent.go` (более 4 тыс. строк),
`mailbox.go` (более 1 тыс.), `coordinator.go` (более 2.7 тыс.), websocket
handlers и session lock. Сейчас существуют минимум три разных detached policy:

- `restartOrphaned` — одна попытка;
- `restartOrphanedWithRetry` — пять попыток, затем in-memory queue;
- `startDetachedRun` — пять попыток, опциональное восстановление DB row.

Именно это дублирование позволило исправить один path и оставить соседний с
другой гарантией. Комментарии очень подробные, но несколько из них уже
противоречат коду: например, `submit` обещает, что `mbReleasing` work будет
передан «still-live turn loop», хотя фактически он становится `orphaned` и
запускается отдельной горутиной; комментарий `startDetachedRun` считает
persisted message достаточной гарантией будущего pickup, которого нет.

Рекомендация после release blockers: выделить единый `SessionExecutor` с
явными состояниями и одним методом `Accept`, а persistence/delivery policy
убрать из coordinator/handler goroutines. Инварианты должны проверяться
табличными state-machine tests, а не зависеть от того, какой из трёх retry
helpers вызвал конкретный exit path.

### P2-4. Очень высокий churn повышает риск очередного регресса

За последние 14 дней в истории находится 299 коммитов; только follow-up к
прошлому отчёту добавил более четырёх тысяч строк, преимущественно тестов и
длинных concurrency-комментариев. Это не дефект само по себе, но для стабильного
релиза означает, что ещё один точечный patch поверх текущей state machine имеет
высокий шанс закрыть одну interleaving и открыть другую.

После исправления P0 рекомендуется короткий stabilization freeze: никаких
новых features, только один канонический executor/queue path, race-oriented
tests и soak/fault-injection на lock release, provider cancellation,
cross-process contention и shutdown.

## Что действительно выглядит исправленным

- Web rerun теперь не удаляет transcript, если session не стала idle за timeout.
- Summary cancel/error cleanup использует bounded cancel-immune context.
- Inline summary использует model/prefix текущего turn snapshot.
- Error exits обычного Run и manual compaction теперь используют handoff
  finalizer и не оставляют очередь без runner при условии, что release и retry
  сами завершились успешно.
- `App.Shutdown` больше не обязан ждать cleanup бесконечно; время возврата имеет
  внешний bound.
- Manual compaction получила idle/hard-cap watchdog.
- Обычные send/rerun handlers больше не всегда публикуют `busy=false` после
  раннего queued return; они перепроверяют mailbox.

Эти улучшения стоит сохранить. Они не компенсируют оставшиеся P0 guarantees.

## Минимальный release gate

Перед стабильным релизом нужны как минимум следующие доказательства:

1. Заблокировать metadata cleanup навсегда; OS-lock освобождается, mailbox
   становится idle, новый call выполняется без разблокировки cleanup.
2. Держать OS-lock дольше текущего retry window, затем отпустить; уже принятый
   web interrupt/orphaned call выполняется **без** дополнительного `Run()` из
   теста или пользователя.
3. Повторить то же для `sessions inject --interrupt`: восстановленная row
   автоматически подхватывается pump-ом после освобождения lock.
4. Поставить второй manual `/compact` во время первого; очередь либо явно
   coalesce-ится, либо второй request завершается, а event trace имеет порядок
   `queued=true -> queued=false`, `busy=true -> busy=false`.
5. Одновременно менять модели двух сессий и запускать manual summary; provider,
   model, options и prefix всегда принадлежат target session.
6. Для каждого provider adapter category hanging stream прекращается после
   cancellation в заданный bound.
7. Shutdown при некооперативном agent не закрывает DB под живым writer и не
   оставляет глобальный DB pool mutex навсегда занятым.
8. Новые тесты проходят с race detector без package-global mutable seams.

Пока пункты 1–3 не выполнены без внешнего «пинка», стабильный release нельзя
считать защищённым от повторения исходного симптома session freeze.
