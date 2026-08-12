# Унификация state machine одного logical call (P2-4)

Дата: 2026-08-12
Точка исследования: `3a145e60b3a0b790456667d7fd9eafe58f8d6fad` (`main`)
Режим: только чтение production-кода. Ничего не менялось, тесты не запускались.
Источник задачи: `docs/reviews/2026-08-12-post-fix-release-readiness-follow-up.md`, P2-4.
Предшественник: `docs/plans/2026-08-10-session-executor-consolidation.md` (план отложен пользователем 2026-08-10; этот документ — обещанный там шаг «re-scope first» + «design the state enum before touching call sites»).

Все ссылки — на **файл + функцию**, не на номера строк: три параллельных агента прямо сейчас правят
`coordinator.go`/`mailbox.go`/`message.go`/`agent.go`, номера сдвинутся, структура — нет.

---

## 0. Резюме для нетерпеливых

Ревьюер прав в диагнозе механики (P0-1 действительно возник из-за того, что решение о владении
принимается в двух местах), но **не прав в выводе, что предложенная целевая модель закрывает
основные findings этого раунда**. Модель закрывает 2 из 3 P0 (P0-1, P0-3) и 1 из 4 P1 (P1-4);
P0-2 она не закрывает вообще — это дефект API `message.Service`, а не state machine.
И самое важное: **основную ценность даёт один из шести шагов** (durable-first accept), а не весь переезд.

Развёрнутая рекомендация — в §8.

---

## 1. Фактическая карта: один logical call от принятия до терминального состояния

### 1.1. Пять независимых «дверей приёма» (accept doors)

Сейчас в системе **пять** мест, каждое из которых может сказать вызывающему «принято», и каждое
даёт **разную гарантию durability**:

| # | Дверь | Точка входа | Что создаётся при «принято» | Durability на момент ответа |
|---|---|---|---|---|
| A | Прямой turn (web/CLI) | `server/handlers.go` → `coordinator.Run`/`RunWithOverrides` → `coordinator.runInternal` → `sessionAgent.Run` → `mailbox.submit` | либо ownership era, либо запись в `mb.submitted` (RAM) | **нет** |
| B | Interrupt из web | `handlers.go` → `coordinator.InterruptAndSend` → `buildCall` → `InterruptAndReplace` **или** `startDetachedRun` | `mb.replacement` (RAM) **или** `session_run_queue` row | **зависит от ветки** |
| C | Cross-process interrupt inject | `cmd/sessions_inject.go` → `pending_injects` row; далее `coordinator.startInterruptTicker` → `handleInterruptTick` | `session_run_queue` row **И** `mb.replacement` | да (и ещё раз да — см. P0-1) |
| D | Не-interrupt inject | `coordinator.InjectMessage` → `sessionAgent.InjectMessage` → `messages.Create` + `mailbox.injectIfBusy` | `messages` row (+ RAM-стамп поколения) | да, но как *сообщение*, не как *call* |
| E | Durable queue | `RunQueuePump.processEntry` → `app.coordinatorAdapterImpl.Run` → `coordinator.RebuildSessionAgentCall` → `RunSessionAgentCall` → `sessionAgent.Run` | lease на существующей row | да |
| F | Orphan handoff | `mailbox.drainOrReleaseFinal` (case 4) / `abandonOwnershipWithHandoff` → `restartOrphanedWithRetry` → `EnqueueRunQueueEntry`, иначе `startBoundedDetachedRun` | `session_run_queue` row **или** только goroutine в RAM | **да или нет** |

Уже здесь видно главное структурное свойство: **«accepted» — это не состояние, а слово**. Оно не
представлено ничем; каждая дверь конструирует свой собственный набор побочных эффектов и возвращает
`nil` наверх.

### 1.2. Детальная трассировка пути C (тот, что породил P0-1)

Это единственный путь, который проходит через *все* слои — coordinator, session service, SQL,
mailbox, agent, message service, app adapter, pump, broker. Разбит по точкам смены состояния.

| Шаг | Где | Что происходит | Кто ВЛАДЕЛЕЦ в этот момент | DURABLE RECORD | CANCELLATION AUTHORITY | TERMINAL? |
|---|---|---|---|---|---|---|
| 1 | `cmd/sessions_inject.go` (чужой процесс) | `messages.Create` + `sessions.CreatePendingInject{Interrupt:true}` | никто (строка сама себе владелец) | `messages` row + `pending_injects` row | нет | нет |
| 2 | `coordinator.startInterruptTicker` | goroutine, тик раз в `interruptInjectTick`, живёт на `tickerCtx`, производном от `runInternal`'s `ctx` | тикер-goroutine (не join-ится ни с чем) | без изменений | `stopTicker` (defer в `runInternal`/`RunSessionAgentCall`) | нет |
| 3 | `coordinator.handleInterruptTick` | `PeekInterruptInject` (SELECT, без удаления) → `messages.Get` → `resolveSessionModels` → `buildCall` → `ToSessionAgentCallData` → `json.Marshal` | по-прежнему строка `pending_injects` | та же строка | tickerCtx | нет |
| 4 | `session.ConsumeInterruptInjectAndEnqueue` (одна tx) | DELETE `pending_injects WHERE id=?` + INSERT `session_run_queue (status='pending')` | **переход владения: `pending_injects` → `session_run_queue` row** | run-queue row, `status=pending`, `attempts=0` | нет (у pending-строки нет владельца) | нет |
| 5 | `coordinator.handleInterruptTick` → `messages.Notify` | публикация в broker | — | — | — | нет |
| 6 | `coordinator.handleInterruptTick` → `sessionAgent.InterruptAndReplace` → `mailbox.interruptAndReplace` | тот же `call` (значение, **без** rowID, **без** `FromDurableQueue`) кладётся в `mb.replacement`; возвращается `mb.current.cancel` | **ДВА ВЛАДЕЛЬЦА ОДНОВРЕМЕННО**: живой turn loop (через `mb.replacement`) и durable row (через `status=pending`) | run-queue row (о ней никто в mailbox не знает) | `mb.current.cancel` (только текущее поколение) | нет |
| 7a | live-ветка: `sessionAgent.Run` loop → `mailbox.reclaimReplacementOrKeep` **или** `runTurn` → `mailbox.drainAfterCancel` **или** `mailbox.drainOrReleaseFinal` | replacement становится `call` следующего поколения | turn loop, era = `epoch`, generation = `mb.current.id` | run-queue row **не тронута** | `mb.current.cancel` (перезаписывается каждым `beginGeneration`) | нет |
| 8a | `runTurn` | `sessions.Get` → `getSessionMessages` → (`createUserMessage` пропускается, т.к. `ExistingMessageID`≠"") → `userMessageCreated=true` | turn loop | `messages` rows | `turnCtx`/`genCtx` cancel + `a.activeRequests[sessionID]` + watchdog | нет |
| 9a | `runTurn` стрим | `messages.Create(assistant)`, `messages.Update` по шагам, `startCheckpoint` пишет partial-снимки | turn loop; checkpoint-goroutine пишет параллельно | `messages` row (`finished_at IS NULL`) | `checkpointWriteCancel`, `genCtx` | нет |
| 10a | `runTurn` финал | `AddFinish(...)` → `messages.Update` с `finished_at != NULL` → `PublishMustDeliver` | turn loop | `messages` row terminal | — | **ТЕРМИНАЛ №1 (сообщение)** |
| 11a | `runTurn` → `drainOrReleaseMerged` → `mailbox.drainOrReleaseFinal` | mbOwned → mbReleasing → (release OS lock) → mbIdle | никто | — | — | **ТЕРМИНАЛ №2 (era)** |
| 7b | durable-ветка (позже): `RunQueuePump.processEntry` | `LeaseRunQueueEntry` → `status=leased, leased_by=pumpInstanceID, lease_expires_at` | pump instance | run-queue row `leased` | `execCtx`/`execCancel` (P1-2 fail-closed watchdog) | нет |
| 8b | `app.coordinatorAdapterImpl.Run` → `RebuildSessionAgentCall` | восстанавливается `SessionAgentCall`, **выставляется `FromDurableQueue=true`**, пересчитываются `ProviderOptions` из ЖИВОГО config (не из снимка!) | pump | та же row | execCtx | нет |
| 9b | `coordinator.RunSessionAgentCall` → `sessionAgent.Run` | если сессия занята → `mailbox.submit` не кладёт в `mb.submitted` (guard `!FromDurableQueue`), Run возвращает `(nil,nil)` → адаптер маппит в `ErrCallQueuedNotExecuted` | pump | row → `NackRunQueueEntryNoAttemptPenalty` → `pending` + локальный `busyBackoffUntil` | — | нет |
| 10b | следующий тик, сессия свободна | тот же provider/tool turn выполняется **второй раз** | pump + новый turn loop | row `leased` | execCtx | нет |
| 11b | `RunQueuePump.executeEntry` | `AckRunQueueEntry` (DELETE, scoped `status='leased' AND leased_by=?`) | — | row удалена | — | **ТЕРМИНАЛ №3 (queue)** |

### 1.3. Что из этого следует читать как контракт

**Три независимых понятия «терминальности»**, ни одно из которых не выводится из другого:

1. **terminal сообщения** — `messages.finished_at IS NOT NULL` (пишет `agent.runTurn` / `message.Service.Update`).
2. **terminal era** — `mailbox.state == mbIdle` при совпадающем `epoch` (пишет `mailbox.drainOrReleaseFinal` / `abandonOwnership*`).
3. **terminal queue** — строка `session_run_queue` удалена через `Ack`/`TerminalFail` (пишет `RunQueuePump.executeEntry`).

Плюс четвёртое, неявное: **возвращённая ошибка** `sessionAgent.Run` → `coordinator` → HTTP-ответ.
`ErrCallAlreadyAttempted` — единственный мостик между (1) и (3), и он односторонний: он говорит
«я уже наследил в `messages`», но не говорит «я закоммитил».

**Четыре реестра cancellation authority**, не упорядоченные между собой:

| Реестр | Устанавливается | Читается | Проблема |
|---|---|---|---|
| `mailbox.current.cancel` | `beginGeneration` (loop + `runTurn`), `beginCompact` | `Cancel`, `interruptAndReplace`, `hardStop` | обнуляется в 6 разных ветках drain'ов именно для того, чтобы «протухший handle» не побеждал fallback |
| `mailbox.dispatcherCancel` | `submit`, `beginCompact` | только `hardStop` (+ fallback в `Cancel`) | по контракту «никогда не цель interrupt», но `Cancel` всё же падает в него |
| `sessionAgent.activeRequests[sessionID]` | `Run` loop + `runTurn` | abort-пути в `OnStepFinish` (max-cost / max-tokens / peak-hours) | **никогда не удаляется**; после первого turn'а сессии там навсегда живёт уже сработавший, инертный cancel |
| `RunQueuePump` `execCancel` | `executeEntry` | renewal goroutine (lease loss / fail-closed timeout) | вообще не известен mailbox'у; отменяет `Coordinator.Run` «снаружи» |

Плюс частные: `checkpointWriteCancel`, `titleCtx`, `tickerCtx`, watchdog (совпадает с `genCtx`).

**Пять независимых счётчиков «поколения»**: `mailbox.epoch` (эра владения), `mailbox.current.id`
(поколение turn'а), `checkpointGeneration` (локальная переменная в `runTurn`, читается/пишется без
согласованной синхронизации), `session_run_queue.attempts`, `config.ConfigStore.Generation`.
Ни один не связан с другим; ни один не попадает в durable-запись call'а.

---

## 2. Перекрытия ответственности (конкретно)

### O-1. Владение решается в двух местах — это и есть механика P0-1

`coordinator.handleInterruptTick` после коммита транзакции делает **и** durable enqueue, **и**
`InterruptAndReplace`. `mailbox.interruptAndReplace` сохраняет `call` в `mb.replacement`
**безусловно** — у него нет ни поля rowID, ни проверки `FromDurableQueue`, ни доступа к
`session.Service`. Guard, который для этого же класса ошибки уже существует в `mailbox.submit`
(`if !call.FromDurableQueue`), сюда не распространяется, потому что это **другая функция того же
файла**, а не общий инвариант.

Ключевая деталь: `call`, построенный в `handleInterruptTick` через `buildCall`, **физически не
может** иметь `FromDurableQueue=true` — этот флаг выставляется исключительно в
`coordinator.RebuildSessionAgentCall`. То есть даже механическая «починка» вида «выставить флаг в
`handleInterruptTick`» не сработала бы: `interruptAndReplace` его всё равно не смотрит.

### O-2. `mailbox.submit` содержит retry-политику — и три из четырёх путей вставки с ней не согласованы

`mailbox` объявлен как ordering/wakeup примитив, но `submit` принимает **durability-решение**:
`if !call.FromDurableQueue { mb.submitted = append(...) }`. Остальные три пути вставки в те же
очереди этого решения не принимают:

- `mailbox.queue` (через `QueueMessage`) — кладёт всегда;
- `mailbox.interruptAndReplace` — кладёт всегда (= O-1);
- `mailbox.reclaimReplacementOrKeep` — возвращает вытесненный `call` **на голову** `mb.submitted` всегда.

**Найдено при этом исследовании (нет в отчёте ревьюера): реальная потеря работы из-за O-2.**
В `agent.runTurn`, в ветке `shouldSummarize`, после инлайновой компактации есть:

```go
if hasPendingToolCalls {
    call.Prompt = fmt.Sprintf("The previous session was interrupted because it got too long, ...", call.Prompt)
    mb.submit(call, nil)   // ← возвращаемое значение отбрасывается
}
```

Здесь `submit` используется **не как «стань владельцем или встань в очередь»**, а как «положи себе
в очередь продолжение». Для call'а, пришедшего из pump (`FromDurableQueue == true`), guard молча
его выбрасывает. Дальше `runTurn` доходит до `drainOrReleaseMerged`, ничего не находит, возвращает
`hasNext=false`, `Run` возвращает `err == nil` → `RunQueuePump.executeEntry` делает **`Ack`
(DELETE строки)**. Продолжение после компактации потеряно без следа, durable row удалена как
«успешно выполненная».

Это ровно тот класс дефекта, который P2-4 описывает: локальная правка (P0-1 guard в `submit`)
не заметила, что `submit` вызывается ещё и не как admission-примитив.

### O-3. Три токена владения без общего fence

| Токен | Область | Кто выдаёт | Кто проверяет |
|---|---|---|---|
| OS session lock | сессия × процесс | `session.TryAcquireSessionLockWithOptions` (в `sessionAgent.Run`) | сам файл-лок + heartbeat |
| mailbox era/generation | сессия × goroutine × процесс | `mailbox.submit`/`beginCompact` (`epoch`), `beginGeneration` (`current.id`) | сравнение `epoch` в drain'ах |
| run-queue lease | строка × pump instance | `LeaseRunQueueEntry` (`leased_by`, `lease_expires_at`) | `WHERE status='leased' AND leased_by=?` в Ack/Nack/Renew |

Ни один не знает про два других. Отсюда законные, но абсурдные состояния: pump **владеет строкой**,
но получает `SessionLockBusyError` — то есть владеет правом на запись, но не правом на выполнение;
и наоборот, live turn loop выполняет работу, чья durable row принадлежит (или будет принадлежать)
другому pump'у.

### O-4. Lease не является fencing token

`AckRunQueueEntry`/`NackRunQueueEntry`/`TerminalFailRunQueueEntry` скоупятся по `leased_by`, то есть
защищают **саму queue-строку**. Но реальные side effects turn'а — `messages.Create`,
`messages.Update`, `sessions.Update` (cost/tokens), запуск tools — **не фенсятся ничем**. Executor,
потерявший lease, узнаёт об этом только через `leaseLost.Load()` **после** возврата
`Coordinator.Run`. Это ровно P1-1 из отчёта, и он не чинится добавлением ещё одного таймера: без
fence, который проверяется *перед каждой persistent-записью*, гарантия остаётся at-least-once.

### O-5. Durable state и in-memory state расходятся в четырёх местах

1. `mb.submitted` / `mb.replacement` — чисто RAM. `hardStop` их **сознательно теряет** (задокументировано в `mailbox.hardStop`).
2. `startBoundedDetachedRun` — принятая работа существует только в памяти 30 секунд (P0-3).
3. **UI busy-state**: `server/handlers.go` выводит `agent_busy` из `AgentCoordinator.IsSessionBusy`, то есть из `mailbox.state`. Сессия с принятой, но ещё не выполненной durable-строкой показывается как **idle**. Пользователь видит «свободно», хотя работа принята.
4. `sessionAgent.activeRequests` — навсегда хранит инертные cancel'ы.

### O-6. Message service «фенсит» в SQL, но не сообщает результат наверх

`internal/db/sql/messages.sql`: `UpdateMessageIfNotTerminal` объявлен как `:exec`, при этом его же
комментарий обещает «Returns number of rows affected». Сгенерированный
`internal/db/messages.sql.go` возвращает только `error`. `message.Service.Update` после `nil`-ошибки
**всегда** публикует переданный снимок. Fence существует на уровне строки БД и не существует на
уровне API. Это P0-2 — и это **не state machine**, это несоответствие сигнатуры контракту.

### O-7. Неоднозначная cancellation authority на abort-путях

`OnStepFinish` (max-cost / max-tokens / peak-hours) отменяет turn через
`a.activeRequests.Get(call.SessionID)` — реестр, который никогда не чистится. Комментарий в
`agent.go` рядом это честно признаёт («entries live forever»). Между двумя turn'ами одной сессии
там лежит уже сработавший cancel предыдущего turn'а; попадание abort'а в это окно = тихий no-op.
Формально это тот же класс, что «пять раз чинившийся stale-cancel-handle» в mailbox'е — но во
втором реестре, где инварианта нет вообще.

### O-8. Lifecycle interrupt-тикера не совпадает с lifecycle того, что он прерывает (P1-4)

`startInterruptTicker` вызывается из `coordinator.runInternal` и `coordinator.RunSessionAgentCall`,
то есть **вокруг одного внешнего вызова** `currentAgent.Run`. Но поколения, которые он должен
прерывать, создаются внутри `sessionAgent.Run`'s turn loop. После первого `fired=true` goroutine
возвращается; replacement-turn (и все последующие turn'ы этого же `Run`) остаются без тикера.
Комментарий у `startInterruptTicker` утверждает обратное («the fresh turn's ticker handles it») —
это исторический текст, а не текущий контракт.

### O-9. Мёртвые протоколы восстановления, поддерживаемые только тестами (P2-2/P2-3)

- `coordinator.requeueInterruptMessage` — production-вызовов нет.
- `coordinator.recreatePendingInjectRowPostAccept` — production-вызовов нет.
- `session.ConsumeInterruptInject` (не-атомарная версия) — не используется production-кодом.
- `SessionAgentCall.InjectID` документирован как «строка, которую надо удалить **после успешного захвата OS lock**» — **ни одна строка `agent.go` этого не делает**. Единственный, кто удаляет по `InjectID`, — `coordinator.startDetachedRun`, и делает это *в начале*, а не после захвата лока. Документированный контракт поля не имеет реализации.

---

## 3. Сопоставление с целевой моделью

Целевая: `accepted -> durable(rowID) -> leased(owner,generation) -> running -> committed | retryable | terminal_failed`

| Состояние модели | Что есть сейчас | Вердикт |
|---|---|---|
| `accepted` | нет представления вообще; пять дверей с разными гарантиями (§1.1) | **отсутствует** |
| `durable(rowID)` | `session_run_queue` — только для путей C/E/F. Пути A/B(live)/D **никогда не получают rowID** | **есть частично**; для основного пути (обычный web/CLI turn) отсутствует структурно |
| `leased(owner, generation)` | `leased_by` + `lease_expires_at` есть; **`generation`/fence нет**. У mailbox'а есть `epoch`+`current.id`, но нет rowID. Две половины токена живут в разных подсистемах и не знают друг о друге | **есть наполовину, разорвано пополам** |
| `running` | не представлено нигде durable. Ответ на «выполняется ли этот call» существует только как «`mb.state == mbOwned`» (внутри процесса) или «`status='leased'`» (внутри pump'а) | **отсутствует** |
| `committed` | три несвязанных терминала (§1.3) | **есть, но трижды и рассогласованно** |
| `retryable` | `attempts` + `Nack`/`NackNoPenalty` — только для queue-origin. Для non-durable call'ов «retryable» = «лежит в `mb.submitted`» | **есть частично** |
| `terminal_failed` | `terminal_failure` + `ErrCallAlreadyAttempted` — только queue-origin. Для прямого turn'а терминальная неудача = возвращённая наверх ошибка, нигде не записанная как состояние call'а | **есть частично** |

Разделение ответственности из отчёта:

| Целевое | Фактическое | Разрыв |
|---|---|---|
| mailbox — только ordering/wakeup | + принимает durability-решение (`FromDurableQueue` guard), + хранит payload'ы, + управляет OS-локом (`drainOrReleaseFinal(release)`), + владеет тремя cancel-функциями, + латч shutdown | 4 лишние обязанности |
| durable store — ownership/retry | ownership только над *строкой*, не над *исполнением*; retry только для queue-origin | ownership неполон |
| message service — versioned/fenced writes | fence есть в SQL, отсутствует в API (`:exec`), версии нет вообще | контракт не выражен в типах |

**Важный положительный вывод:** целевая модель — это не green-field. `session_run_queue`
(pending/leased/attempts/terminal_failure/idempotency key) — это уже примерно 70 % durable-половины.
`mailbox.epoch`/`current.id` — уже приличный ordering-примитив. Не хватает трёх связок:
(a) rowID для *каждого* принятого call'а, (b) единый owner token, который знает и rowID, и era,
(c) fenced write в message service.

---

## 4. Объём миграции

Измерено на `3a145e60`:

| Метрика | Значение |
|---|---|
| Литералы `SessionAgentCall{}` | 32 в production, 278 всего (246 в тестах) |
| Call sites методов `mailbox.*` | 23 в production (все в `agent.go`), 27 тестовых файлов обращаются к internals напрямую |
| Call sites run-queue сервиса | 25 в production |
| Двери приёма | 5 (§1.1) |
| Реестры cancel | 4 (§1.3) |
| Тестовых файлов в `internal/agent` + `internal/session` | 100 |
| Тестовых функций там же | 407 |

**Что можно делать инкрементально:** почти всё. Ключ — вводить `ticket{rowID, sessionID,
logicalCallID, fence}` как *дополнительное* поле рядом с существующими, а не вместо них; менять
одну дверь за раз; guard'ы старых путей оставлять до последнего шага.

**Что требует schema-миграции БД:**

- `ALTER TABLE session_run_queue ADD COLUMN fence INTEGER NOT NULL DEFAULT 0` — аддитивно, безопасно для SQLite.
- `ALTER TABLE session_run_queue ADD COLUMN logical_call_id TEXT` — опционально; сейчас идентичность зашита в `id` как `"<sessionID>-<logicalCallID>"`, этого фактически хватает.
- Изменение `CHECK(status IN ('pending','leased','acked'))` под новые состояния (`running`, `committed`) — **требует пересоздания таблицы** в SQLite. **Рекомендация: не трогать `status`.** Ввести отдельную колонку `phase TEXT` или обойтись существующими тремя состояниями + `fence`. Экономит целую рискованную миграцию.
- `messages`: если делать по-настоящему versioned writes — `ADD COLUMN version INTEGER NOT NULL DEFAULT 0`. Для закрытия P0-2 **этого не требуется** (достаточно `:execrows`).

**Где максимальный риск регрессии:**

1. `mailbox.drainOrReleaseFinal` — атомарность «mbIdle не раньше, чем отпущен OS-лок» (HIGH-1) плюс `mbReleasing`-окно плюс case 4 (orphaned). Это самый дорого добытый инвариант в файле; любая реструктуризация может его тихо открыть обратно.
2. `hardStop` / shutdown-семантика: где именно discard, где сохранение очереди. Сейчас это размазано по 5 веткам с разными решениями.
3. Epoch-guard'ы (`if mb.epoch != epoch { return }`), которые абсорбируют безусловный `defer` в `Run`. Если ввести ticket, надо решить, что становится «стейл»: era или ticket.
4. Тесты: 27 файлов знают internals mailbox'а, и как минимум два (`coordinator_test.go:TestHandleInterruptTick`, `p0_2_fault_injection_test.go`) **фиксируют текущее ошибочное поведение** — их придётся переписать, а не «сохранить зелёными».

---

## 5. Поэтапный план (независимо мержибельные шаги)

Шаги упорядочены по убыванию отношения «ценность / риск». Каждый шаг — отдельный PR со своим gate.

### Шаг 0 — гигиена карты (нулевой риск, полдня)

Удалить мёртвые пути O-9 (`requeueInterruptMessage`, `recreatePendingInjectRowPostAccept`,
`ConsumeInterruptInject`, тесты на них), удалить/реализовать документированный контракт
`SessionAgentCall.InjectID`, привести к реальности 4 комментария из P2-3.

*Делает структурно невозможным:* ничего. Но делает следующие шаги обозримыми: сейчас невозможно
отличить «этот путь живой» от «этот путь поддерживается тестом».
*Gate:* сборка + `go test ./internal/agent/... ./internal/session/...`, diff не трогает production-логику.

### Шаг 1 — durable-first accept (главный шаг; **вся основная ценность здесь**)

Ввести один admission-примитив в `session.Service`:
`AcceptCall(ctx, sessionID, logicalCallID, callData) (ticket, error)` — INSERT `pending` +
(в той же транзакции, если принимающий процесс собирается выполнять сам) `LeaseRunQueueEntry` под
собственный instance id. Провести через него **все пять дверей** §1.1.
`startBoundedDetachedRun` удаляется целиком.

*Делает структурно невозможным:*
- принятый call, существующий только в памяти процесса → **P0-3 закрыт по построению**;
- потерю `mb.submitted`/`mb.replacement` при `hardStop` (документированная сегодня потеря);
- SEC-1 как побочный эффект (исчезает функция, логирующая `call.Prompt` на ERROR);
- невидимость принятой работы для UI (busy можно считать из durable-состояния).

*Цена:* +2 SQLite-записи на каждый turn (INSERT+DELETE); в steady state таблица остаётся пустой.
*Gate:* для каждой из 5 дверей — тест «после возврата `accepted` в `session_run_queue` есть строка»;
тест «процесс убит между accept и run → pump подхватывает»; существующие сюиты зелёные.
*Риск:* средний. Основной — производительность на длинных сессиях и поведение при недоступной БД
(сейчас часть путей деградирует в RAM, после шага 1 они должны честно возвращать ошибку).

### Шаг 2 — единый owner token (rowID + fence)

`ALTER TABLE ... ADD COLUMN fence INTEGER`. `LeaseRunQueueEntry`/`RenewRunQueueLease` инкрементят и
возвращают fence. `mailbox` хранит `ticket` вместо голого `SessionAgentCall`; era (`epoch`) и
lease становятся **двумя проекциями одного токена**. `interruptAndReplace` принимает ticket и
**отказывается** принимать call, чей lease не принадлежит этому процессу.

*Делает структурно невозможным:* **P0-1**. Живой owner физически не может держать call, чья
durable-строка не зализана этим процессом; двойного исполнения нет, потому что нет второго владельца.
*Gate:* минимальный release gate №1 из отчёта — один inject → одна queue row → **ровно одно**
provider/tool execution, включая busy-owner case; счётчик исполнений в тесте, а не косвенные проверки.

### Шаг 3 — mailbox теряет durability-политику

Убрать `FromDurableQueue` из `mailbox.submit`. Mailbox оперирует только ticket'ами и отвечает
исключительно за порядок и пробуждение. «Не могу выполнить сейчас» → возврат lease в durable store
(`NackNoPenalty`), а не хранение копии в RAM. `mb.submit(call, nil)` из `shouldSummarize`-ветки
заменяется на явный «продли ticket» / «создай продолжение».

*Делает структурно невозможным:* найденную в §O-2 тихую потерю продолжения после компактации;
рассогласование четырёх путей вставки; необходимость в `busyBackoffUntil`-эвристике pump'а.
*Gate:* table-driven тест переходов mailbox'а (то, что рекомендовал ещё план 2026-08-10) +
инвариант «mailbox никогда не держит ticket без действующего lease этого процесса».

### Шаг 4 — fenced/observable writes в message service (независим от 1–3)

`UpdateMessageIfNotTerminal` → `:execrows`; `message.Service.Update` возвращает `(applied bool, err error)`
и публикует **только при `applied`**. Синхронизировать `checkpointGeneration`/`checkpointPartsLen`
одним примитивом.

*Делает структурно невозможным:* **P0-2** (stale publish). Но см. §6: это НЕ следствие целевой
модели, это самостоятельная правка, и её надо делать независимо от решения о рефакторинге.
*Gate:* тест порядка «terminal publish → поздний conditional checkpoint» с проверкой и БД, и
последнего события broker'а; `-race` на перекрытии поколений.

### Шаг 5 — единая cancellation authority

Свернуть `activeRequests` + `mb.current.cancel` + `mb.dispatcherCancel` + pump'овский `execCancel`
в один per-era объект-владелец с методами `cancelGeneration()` / `cancelEra()`. Interrupt-watcher
становится session-level и joinable, регистрируется в том же объекте. Lease-watchdog (P1-1) —
таймер того же объекта, а не отдельная goroutine внутри `executeEntry`.

*Делает структурно невозможным:* **P1-4** (тикер, не совпадающий с lifecycle); O-7 (протухшие
cancel'ы в `activeRequests`); делает P1-1 реализуемым в одном месте вместо трёх.
*Gate:* два быстрых interrupt подряд прерывают две последовательные generation в FIFO-порядке;
`CancelAll` join-ит interrupt/checkpoint/pump writers.

### Шаг 6 — разбиение файлов (**делать последним и только если 1–5 сделаны**)

Вынести owner/executor в отдельный пакет. Это косметика: заголовочная жалоба P2-4 («5046 строк»)
сама по себе ничего не чинит, и разбиение файлов **до** шагов 1–3 только размажет ту же путаницу
по большему числу файлов.

---

## 6. Какие findings раунда целевая модель реально закрывает (честно)

| Finding | Закрывается моделью? | Каким шагом | Комментарий |
|---|---|---|---|
| **P0-1** двойное владение interrupt'ом | **ДА, структурно** | 2 (+3) | Единственный owner token; второй владелец невыразим в типах |
| **P0-2** stale publish позднего checkpoint | **НЕТ** | 4 (самостоятельный) | Это дефект сигнатуры `:exec`/`Update`, не state machine. Чинится ~10 строками СЕГОДНЯ, без всякого рефакторинга. Модель лишь делает «fenced write» явным контрактом, снижая вероятность рецидива |
| **P0-3** потеря accepted call | **ДА, структурно** | 1 | Ровно определение шага 1. Это самый сильный аргумент за рефакторинг — и он достигается **одним** шагом из шести |
| **P1-1** lease fail-closed после истечения | Частично | 5 (+2 для fence) | Модель даёт место, куда положить watchdog и fence; сама по себе не чинит. Строгий exactly-once без fence в persistent writes недостижим |
| **P1-2** 401 retry со старым pinned client | **НЕТ** | — | Ортогонально: слой моделей/конфига |
| **P1-3** неатомарный config snapshot | **НЕТ** | — | Ортогонально: `config.ConfigStore` |
| **P1-4** ticker lifecycle | **ДА** | 5 | Но чинится и локально (привязать тикер к turn loop + join через WaitGroup, ~50 строк) |
| **P2-1** FIFO tie-breaker | **НЕТ** | — | 2 строки SQL |
| **P2-2** мёртвые recovery paths | Нет — закрывается удалением | 0 | Модель мешает им **отрасти заново** (одна дверь приёма вместо пяти), но не удаляет существующие |
| **SEC-1** raw prompt в ERROR-логе | Побочно ДА | 1 | Функция-нарушитель (`startBoundedDetachedRun`) исчезает вместе с fallback'ом |
| Найденное здесь: тихая потеря продолжения после компактации (§O-2) | **ДА, структурно** | 3 | Новый, не описанный в отчёте дефект того же класса |

**Итог по пункту 6.** Из трёх release-блокеров модель структурно убивает два (P0-1, P0-3) и не
трогает третий (P0-2). Из четырёх P1 — убивает один (P1-4), делает решаемым один (P1-1), не трогает
два. Это **не** «рефакторинг закроет весь раунд». Это «рефакторинг закроет два самых дорогих
блокера так, что они не смогут вернуться, а остальные придётся чинить точечно в любом случае».

---

## 7. Честная оценка альтернатив

### Альтернатива A — «один владелец после commit», ~30 строк

Ровно то, что рекомендует сам отчёт как простейший контракт: в `handleInterruptTick` после коммита
**не вызывать** `InterruptAndReplace` с payload'ом; вместо этого отменить текущее поколение
(`Cancel`) и разбудить pump.

- Закрывает: P0-1 сегодня.
- Цена: latency interrupt'а растёт до тика pump'а (3 с) плюс возможный `busyBackoffUntil` (до одного lease TTL = 30 с) — это ощутимая UX-регрессия для «прерви и запусти вот это».
- Риск: низкий по коду, средний по продукту.
- Тесты: два существующих теста фиксируют текущее (ошибочное) поведение и должны быть переписаны — от этого не уйти ни в одном варианте.

### Альтернатива B — только шаг 1 (durable-first accept)

- Закрывает: P0-3 структурно, SEC-1 побочно, снимает документированную потерю очереди при shutdown, **и снимает UX-минус альтернативы A**: живой процесс лизует собственную строку и выполняет её немедленно, без ожидания тика pump'а.
- Не закрывает: P0-1 (нужен шаг 2 — fence — чтобы live-владение и durable-владение были одним токеном).
- Оценка: это ~60–70 % ценности всего переезда за ~25 % его объёма.

### Альтернатива C — локальный набор точечных правок без архитектуры

P0-2 (`:execrows`), P1-4 (тикер в turn loop), P2-1 (SQL tie-breaker), P2-2 (удаление), SEC-1 (лог),
плюс альтернатива A для P0-1, плюс durable-марка для P0-3 без общего accept-примитива.

- Закрывает формально весь список.
- **Но:** именно так были закрыты предыдущие два раунда, и каждый раз локальная правка одного
  владельца не замечала второго. Механика P2-4 подтверждается историей файла: пять раундов подряд
  чинили «stale cancel handle» по одной ветке за раз (это буквально написано в комментариях
  `mailbox.go`: «the fourth instance», «the sixth instance of the same shape»). Ставить на то, что
  шестая итерация локальных правок сойдётся, — против имеющихся данных.

### Сравнение

| Вариант | Объём | Закрывает P0 | Риск регрессии | Останавливает рецидивы |
|---|---|---|---|---|
| C (только точечно) | ~1–2 дня | 3/3 формально | низкий на правку, **высокий кумулятивно** | нет |
| A (один владелец) | ~0.5 дня | P0-1 | низкий | нет |
| B (шаг 1) | ~3–5 дней | P0-3 | средний | для класса «потеря принятого» — да |
| Шаги 1+2+3 | ~2–3 недели | P0-1, P0-3 | высокий на шаге 2 | да |
| Шаги 0–6 целиком | ~1.5–2 месяца | P0-1, P0-3 | высокий | да |

---

## 8. Резюме и рекомендация

**Делать частично.**

1. **Не гейтить релиз рефакторингом.** Для релиза достаточно варианта C + A: P0-2 закрывается
   `:execrows` (10 строк), P1-4 — привязкой тикера к turn loop, P0-1 — отказом от `mb.replacement`
   после durable enqueue, P0-3 — durable-маркой перед возвратом «принято». Это дни, не недели.
2. **Затем сделать шаг 1 (durable-first accept) как отдельный, профинансированный инкремент.** Это
   единственный шаг, который даёт структурную, а не декларативную гарантию: «принятая работа не
   существует только в памяти». Он же убирает `startBoundedDetachedRun` (SEC-1) и делает
   UI-состояние выводимым из durable-состояния.
3. **Шаги 2–3 (единый owner token + очистка mailbox) — только если после шага 1 P0-1-класс
   всплывёт снова.** Они дороже и рискованнее всего остального вместе взятого, потому что задевают
   `drainOrReleaseFinal` — самый дорого добытый инвариант в кодовой базе.
4. **Шаг 6 (разбиение файлов) не делать как самостоятельную цель.** Заголовок P2-4 («5046 строк»)
   вводит в заблуждение: проблема не в размере файлов, а в том, что «владелец» — это пять разных
   слов в пяти подсистемах. Разбиение файлов до унификации владения только размажет это шире.
5. **Тесты — реальная стоимость, а не код.** 27 тестовых файлов знают internals mailbox'а, минимум
   два фиксируют ошибочное поведение. Любой из вариантов выше требует их переписать; шаги 2–3
   требуют переписать их массово.

Отрицательная часть вывода, которую стоит зафиксировать явно: **целевая модель, как она
сформулирована в отчёте, не закрывает P0-2, P1-2, P1-3 и P2-1 вообще**, и не является условием
закрытия P1-1. Аргумент «рефакторинг закроет findings» верен ровно для двух из них. Аргумент,
который действительно работает, — другой: рефакторинг делает невозможным **повторное появление**
класса «два владельца / потерянная принятая работа», а история этого файла показывает, что этот
класс возвращается в каждом раунде.
