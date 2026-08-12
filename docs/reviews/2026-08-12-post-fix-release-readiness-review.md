# Повторное ревью готовности релиза после multi-agent fixes

Дата: 2026-08-12  
Режим: статическое ревью production-кода и истории Git; тесты, сборка, lint и race detector не запускались.  
Точка ревью: `684e79fb8e8de5ab317537f0c7b8d51e2e1fcdce` (`main`).  
База сравнения: `78f2090c9f9eb63560308cd6eb0247b4a9471e39` — HEAD предыдущего release-readiness review.  
Диапазон: `78f2090c..684e79fb`, 25 коммитов, 53 файла, `+6128/-300`.  
Не входили в оценку пользовательские изменения: удалённый `web/dist/.gitkeep` и untracked `dev/`.

## Итог

**Вердикт: NO-GO для стабильного релиза.**

Работа последнего раунда не была напрасной: основной рекурсивный self-deadlock исчез, mailbox стал заметно строже, `Run`/`Summarize`/title вошли в shutdown join, pump получил admission gate, worker limit и owner-scoped outcome, lease-loss теперь отменяет execution context, а исходный сценарий двойного помещения durable call в busy mailbox закрыт.

Я не нашёл нового очевидного цикла вида `mutex A -> mutex B -> mutex A` в основном mailbox path. Однако отсутствие такого цикла ещё не означает отсутствие freeze: в текущем HEAD остаются неограниченные ожидания, поздние DB writers и неатомарные durability handoff. Кроме того, два последних исправления exactly-once/idempotency не проходят через весь жизненный цикл вызова.

Подтверждены три release blocker:

1. `LogicalCallID` генерируется, но теряется при сериализации в durable queue. При handoff через replacement/finalizer один логический вызов снова превращается в две durable rows и может выполниться дважды.
2. Cross-process interrupt удаляется из `pending_injects` до гарантированного помещения в mailbox или durable queue. Любая ошибка между consume и handoff теряет событие; recovery использует тот же уже отменённый context.
3. Checkpoint writer после пятисекундного timeout перестаёт join-иться, хотя всё ещё имеет право писать полную старую копию message. `Run()` и `CancelAll()` могут закончиться, БД — закрыться, а поздняя запись — затереть terminal finish более старым partial snapshot.

Кроме них остаются P1-проблемы: `RunQueuePump.Stop()` содержит неограниченный `p.wg.Wait()`, lease renewal продолжает реальную работу после ошибок renewal до явного `!ok`, model cache не инвалидируется при изменении credentials/config, а INFO-логи CLI раскрывают prompt.

## Что действительно исправлено

| Предыдущее замечание | Статус на `684e79fb` |
|---|---|
| P0-1: pump копирует durable call в mailbox занятой сессии | **Закрыто для исходного сценария.** `FromDurableQueue` заставляет `mailbox.submit` не добавлять копию, adapter возвращает `ErrCallQueuedNotExecuted`, row остаётся durable. Появился другой duplicate-path из-за потери `LogicalCallID`, см. P0-1 ниже. |
| P0-2: accepted call теряется до durable commit | **Частично.** Enqueue стал синхронным и ошибки возвращаются. Orphan-finalizer игнорирует ошибку и оставляет runnerless fallback; interrupt handoff всё ещё удаляет исходный marker слишком рано. |
| P0-3: `RunQueuePump.Stop` не join-ит workers | **Основная гонка закрыта.** `workerWg`, admission gate и forced-shutdown policy добавлены. Main pump loop всё ещё ожидается без timeout. |
| P0-4: shutdown не join-ит manual summary/title | **Закрыто для `Summarize` и title.** Остаётся checkpoint goroutine, которую `Run` сознательно бросает после timeout. |
| P1-1: `WaitGroup.Add` одновременно с `Wait` | **Закрыто.** `admitMu` атомарно объединяет проверку shutdown и регистрацию `Run`/`Summarize`; аналогичный gate есть у pump workers. |
| P1-2: executor продолжает работать после потери lease | **Частично.** Явный `!ok` отменяет `execCtx`; ошибки renewal только логируются, поэтому после истечения TTL возможен overlap до следующего успешного renewal. |
| P1-3: FIFO неоднозначен в пределах секунды | **Закрыто.** Добавлен `rowid ASC` после `created_at ASC`. |
| P1-4: неограниченный fan-out pump | **Закрыто.** `execSem` ограничивает глобальное число executions до 10. |
| P1-5: stale cleanup стирает metadata нового lock owner | **Сильно сужено, но не доказано абсолютно.** Generation sidecar отсекает уже видимого нового owner; код прямо документирует остаточный compare-then-delete TOCTOU. Это diagnosability-риск, не новый OS-lock deadlock. |
| P2-2: recursion guard обходится через `env -S` | **Закрыто статически.** Wrapper normalization унифицирован. |
| P2-3: Codex MCP token находится в argv | **Закрыто для token.** Token передаётся через environment в обязательном pipe path Codex. Отдельно остаётся утечка prompt в INFO args log. |
| P2-4: SSRF пропускает special-purpose ranges | **Закрыто в заявленном объёме.** Dial-time `Control` и дополнительные CIDR покрывают проверенный класс; proxy при включённом guard отключён. |
| P2-5: handoff между mailbox eras неатомарен | **Закрыто.** `abandonOwnershipAndPopSubmitted` выполняет проверку epoch, release и pop одной critical section. |

## Release blockers

### P0-1. Stable logical ID не переживает durable boundary — возможен повторный запуск

Коммит `e56cacdb` добавил `SessionAgentCall.LogicalCallID` и строит idempotency key как `sessionID + LogicalCallID` (`internal/agent/agent.go:357-367`, `internal/agent/agent.go:1175-1185`, `internal/agent/coordinator.go:2461-2473`). Само хранилище корректно делает `ON CONFLICT(id) DO NOTHING` (`internal/db/sql/run_queue.sql:1-19`).

Но ID отсутствует в durable DTO:

- `SessionAgentCallData` не содержит `LogicalCallID` (`internal/session/session.go:1343-1375`);
- `ToSessionAgentCallData` его не записывает (`internal/agent/call_data_conversion.go:55-81`);
- `RebuildSessionAgentCall` его не восстанавливает (`internal/agent/coordinator.go:2580-2605`).

Следовательно, любой вызов, поднятый pump из БД, снова имеет пустой `LogicalCallID`.

Достижимая цепочка дублирования:

1. Pump арендует durable row A и восстанавливает call с `FromDurableQueue=true`, но с пустым `LogicalCallID`.
2. Во время preamble приходит interrupt replacement D. `reclaimReplacementOrKeep` отдаёт D следующим, а A возвращает в начало `mb.submitted` (`internal/agent/mailbox.go:929-936`).
3. D завершается ошибкой до обычного drain. Финальный `abandonOwnershipWithHandoff` извлекает оставшийся A и вызывает `restartOrphanedWithRetry` (`internal/agent/agent.go:1077-1089`).
4. Из-за пустого ID recovery создаёт для A новый timestamp key (`internal/agent/agent.go:1178-1185`) и новую durable row.
5. Исходный pump получает ошибку от всего dispatcher и Nack-ает первоначальную row A обратно в pending (`internal/session/run_queue_pump.go:923-927`). Теперь в очереди находятся две rows одного логического вызова.

Тесты проверяют два вызова `restartOrphanedWithRetry` на одном уже заполненном `SessionAgentCall.LogicalCallID`, но не round-trip `SessionAgentCall -> JSON -> Rebuild -> orphan handoff` (`internal/agent/p2_1_idempotency_key_test.go:68-99`). Поэтому текущая проверка проходит, не затрагивая место потери ID.

Что исправить:

- сериализовать `LogicalCallID` в `SessionAgentCallData` и восстанавливать его;
- не создавать вторую row для orphaned call с `FromDurableQueue=true`: исходная leased row должна оставаться единственным recovery record либо её ID должен явно путешествовать вместе с call;
- добавить end-to-end regression на описанный replacement/error/handoff сценарий с подсчётом provider turns и rows.

### P0-2. Cross-process interrupt удаляется до гарантированного handoff

`handleInterruptTick` сначала вызывает `ConsumeInterruptInject`, который в одной транзакции читает и **удаляет** row (`internal/session/session.go:1148-1184`), и только затем выполняет `requeueInterruptMessage` (`internal/agent/coordinator.go:2319-2329`).

После commit удаления до гарантированного handoff остаются fallible операции:

- чтение message (`internal/agent/coordinator.go:2342-2346`);
- построение call/model/provider (`internal/agent/coordinator.go:2348-2351`);
- `InterruptAndReplace` либо durable enqueue (`internal/agent/coordinator.go:2361-2365`);
- JSON marshal и запись run queue (`internal/agent/coordinator.go:2476-2502`).

Ошибка в первых двух шагах вообще не воссоздаёт marker. Ticker логирует ошибку и продолжает polling, но row уже удалена, поэтому следующему tick нечего повторять.

Даже enqueue fallback ненадёжен: `recreatePendingInjectRow` читает message и создаёт новую row на том же `ctx` (`internal/agent/coordinator.go:2511-2535`). Если enqueue не состоялся из-за cancellation/deadline, recovery с тем же context также гарантированно или с высокой вероятностью не состоится. Это особенно реально при гонке завершения owning turn: ticker context связан с жизнью turn (`internal/agent/coordinator.go:2283-2303`).

Для web idle-interrupt обратная проблема — context намеренно превращён в `context.WithoutCancel` (`internal/server/handlers.go:376-377`), а `startDetachedRun` не добавляет собственного timeout. Заблокированная SQLite write может зависнуть без верхней границы.

Что исправить:

- не моделировать handoff как destructive consume + best-effort recreation;
- ввести состояние/lease для `pending_injects` (`pending -> claimed -> durably_enqueued/consumed`) либо атомарную транзакционную передачу в `session_run_queue`;
- все post-accept recovery writes выполнять на отдельном bounded context;
- возвращать и сохранять ошибку recreation, а не только логировать её;
- добавить fault-injection на ошибку `messages.Get`, ошибку model build, canceled context и ошибку enqueue после успешного consume.

### P0-3. Orphan recovery всё ещё может оставить accepted call без runner

`restartOrphanedWithRetry` теперь действительно ждёт все enqueue и возвращает первую ошибку (`internal/agent/agent.go:1161-1232`). Но production finalizer вызывает его без проверки результата (`internal/agent/agent.go:1077-1089`). Другой wrapper только пишет log (`internal/agent/agent.go:1129-1132`).

При ошибке enqueue функция кладёт call через `mailbox.queue` в уже idle mailbox (`internal/agent/agent.go:1207-1215`). Этот API сам документирует, что runner появится лишь при каком-нибудь будущем `submit`; recovery runner немедленно не запускается (`internal/agent/mailbox.go:1141-1151`). Если нового сообщения не будет или процесс завершится, accepted call останется только в памяти и потеряется.

То есть commit `56238306` улучшил наблюдаемость функции и синхронность, но не восстановил end-to-end гарантию: ошибка дошла только до finalizer, который уже не способен сообщить её исходному caller и игнорирует return value.

Что исправить:

- убрать runnerless fallback;
- держать ownership/handoff marker до успешного durable commit либо запускать гарантированный bounded runner с явным lifecycle join;
- изменить finalizer так, чтобы ошибка влияла на возвращаемый результат/terminal state, а не исчезала после log;
- тестировать не только return value helper-функции, а реальный `Run` finalizer при отказавшем enqueue.

### P0-4. Checkpoint goroutine может пережить `Run`, закрытие БД и затереть terminal message

На каждом step запускается checkpoint goroutine, которая периодически записывает полную копию `currentAssistant` с partial finish (`internal/agent/agent.go:2076-2129`). `stopCheckpoint` ждёт её не более пяти секунд, после чего просто забывает `checkpointDone` (`internal/agent/agent.go:2131-2145`).

Если goroutine в этот момент зависла в `messages.Update`:

1. `stopCheckpoint` возвращается по timeout.
2. `runTurn` продолжает terminal/error writes и возвращается.
3. `Run` делает `runWg.Done`; `CancelAll` считает agent полностью завершённым.
4. `App.Shutdown` может выбрать graceful branch и закрыть БД.
5. Старая goroutine всё ещё является DB writer. Если она поздно разблокируется до close, `message.Update` целиком перезапишет `parts` и `finished_at` (`internal/message/message.go:256-279`) более старым partial snapshot. Если после close — получит ошибку уже за пределами lifecycle join.

При следующем step после timeout также может стартовать новая checkpoint goroutine, пока старая ещё жива. Обе используют общий `checkpointPartsLen` без отдельной синхронизации; старый writer способен завершиться после нового и нарушить порядок snapshots.

Это прямое нарушение обещания shutdown: «после graceful return нет живых DB writers».

Что исправить:

- дать checkpoint write отдельный bounded/cancelable context и отменять его при stop;
- не разрешать следующему checkpoint стартовать, пока предыдущий writer не завершён либо не fenced;
- добавить version/sequence в conditional `UpdateMessage`, чтобы старый snapshot физически не мог заменить более новый terminal state;
- учитывать checkpoint workers в общем lifecycle registry, а forced timeout отражать в `stillBusy`, запрещая DB close.

## Высокий приоритет

### P1-1. `RunQueuePump.Stop` всё ещё способен зависнуть навсегда

Пятисекундный timeout покрывает только `workerWg` (`internal/session/run_queue_pump.go:377-398`). Если workers уже закончились, код без timeout вызывает `p.wg.Wait()` (`internal/session/run_queue_pump.go:386-392`).

Main `run()` выполняет `tick()` синхронно, а внутри него находятся DB-вызовы `CleanupExpiredLeases` и `ListPendingRunQueueEntries` на cancelable, но не deadline-bounded `p.ctx` (`internal/session/run_queue_pump.go:401-456`). Если драйвер/FS/SQLite не выйдет по cancellation, `Stop()` и затем `App.Shutdown()` зависнут навсегда. Комментарий в `App.Shutdown` о bounded shutdown в этом случае неверен.

Нужен единый timeout на join main loop + workers и forced result для любого незавершённого компонента, не только worker.

### P1-2. Ошибка lease renewal допускает параллельное выполнение старого и нового owner

На `RenewRunQueueLease` с `err != nil` код только пишет warning и продолжает выполнение (`internal/session/run_queue_pump.go:766-773`). `execCancel` вызывается лишь при следующем renewal, который успешно ответил `ok=false` (`internal/session/run_queue_pump.go:774-793`).

Если renewal не удаётся дольше TTL, другой процесс может cleanup-нуть row, получить lease и начать второй execution. Первый executor узнает об этом только на более позднем успешном renewal; до этого оба выполняют LLM/tools/DB side effects одновременно. Даже после cancel provider/tool может не остановиться мгновенно.

Это не классический mutex deadlock, но один из наиболее опасных остаточных concurrency hazards. Минимально нужно отслеживать `lastSuccessfulRenewal` и fail-closed отменять execution до истечения собственного safety deadline. Для строгой exactly-once семантики необходим fencing token на persistent side effects; текущая queue по сути остаётся at-least-once.

### P1-3. Model cache сохраняет старые credentials и неполный `SelectedModel`

`resolveSessionModels` кэширует готовые `Model`/provider clients по ключу только из `provider:model:reasoning_effort` для large и small (`internal/agent/coordinator.go:659-684`). В ключ не входят:

- `Think`, `MaxTokens`, temperature/top-p/top-k/penalties и `ProviderOptions` из `SelectedModel`;
- API key/OAuth token, base URL, headers и прочая provider config;
- generation/version `ConfigStore`.

Cache нигде не очищается: `UpdateModels` пересобирает только shared `currentAgent` (`internal/agent/coordinator.go:2836-2859`). После config reload, смены endpoint/options или OAuth refresh `resolveSessionModels` продолжает возвращать старый `fantasy.LanguageModel` и старый `ModelCfg`.

Это ломает и 401 retry: `runInternal` пинит cached model в `agentCall` (`internal/agent/coordinator.go:965-995`), `runWithUnauthorizedRetry` обновляет shared models, но повторно вызывает ту же closure с тем же pinned call (`internal/agent/coordinator.go:3005-3017`, `internal/agent/coordinator.go:3052-3060`). Текущий turn может повторить запрос тем же устаревшим client/token, а последующие turns снова получить его из cache.

Нужно либо кэшировать только immutable provider-independent metadata, либо включить полный config fingerprint/generation и очищать cache атомарно при любом config/credential update. 401 retry должен заново resolve/build/pin call после refresh.

### P1-4. Некоторые session-scoped paths всё ещё откатываются к shared model

`buildCall` при `pinned == nil` читает `c.currentAgent.Model()` (`internal/agent/coordinator.go:834-842`), хотя комментарий утверждает, что после isolation fix каждый caller передаёт snapshot. Это утверждение неверно: cross-process `requeueInterruptMessage` вызывает `buildCall(..., nil, nil)` (`internal/agent/coordinator.go:2342-2349`). `InterruptAndSend` также оставляет `pinned=nil`, если overrides не переданы (`internal/agent/coordinator.go:2384-2393`).

Web handler обычно вручную восстанавливает overrides из session DB, поэтому основной UI path частично защищён. Но coordinator contract и cross-process path не защищены: сессия с persisted model override может получить current/global model, а вместе с ним другой context window, provider options и attachment capability.

Защитный fallback должен вызывать `resolveSessionModels(sessionID)`, а не читать shared agent. Лучше убрать nullable snapshot из production API полностью.

## Security и наблюдаемость

### SEC-1. CLI provider пишет полный prompt в INFO log через `args`

В PTY path prompt добавляется в argv, после чего `strings.Join(args, " ")` целиком пишется на INFO (`internal/agent/cliprovider/provider.go:1165-1181`). Поля `promptHead`/`promptTail` создают впечатление ограниченного логирования, но `args` уже содержит полный system prompt, историю, пользовательский текст и потенциальные секреты из context/files.

Pipe path также пишет head/tail prompt на INFO (`internal/agent/cliprovider/provider.go:1311-1322`). Исправление Codex token устранило один конкретный secret из argv, но общий канал утечки остался.

В production INFO следует логировать только binary, безопасный список имён flags, counts/length/hash. Prompt samples и raw argv допустимы лишь в явно включаемом redacted diagnostic режиме.

### P2-1. Lock metadata generation guard остаётся best-effort

`clearHolderMetadata` сначала читает `.gen`, затем отдельно открывает/truncate-ит lock file и удаляет sidecars (`internal/session/lock.go:523-580`). Между compare и destructive operations новый owner всё ещё может записать свою generation. Код честно признаёт gap (`internal/session/lock.go:510-522`).

Это больше не удерживает OS lock и поэтому не является найденной причиной freeze. Но `sessions kill/why/locks` может кратковременно получить стёртый PID нового holder. До stable release достаточно либо явно классифицировать это как принятый diagnostic risk, либо перестать разрушительно чистить shared filename после unlock и хранить immutable owner-versioned metadata.

## Общий обзор качества кода

### Сильные стороны

- Mailbox epoch/state machine и атомарные drain/release/handoff существенно лучше прежней смеси recursive `Run`, очередей и отдельных busy flags.
- Fail-closed OS locking, owner-scoped queue mutations и cancellation на подтверждённую потерю lease — правильные safety defaults.
- Pump теперь имеет глобальный worker bound, admission gate, fresh bounded contexts на outcome writes и корректный FIFO tie-breaker.
- Session-specific model snapshots устранили запись per-session выбора в shared agent на основном path.
- В последних коммитах много полезных негативных tests и подробных исторических объяснений причин исправлений.

### Системные запахи

- Runtime-файлы слишком велики: `agent.go` — около 4.9k строк, `coordinator.go` — 3.4k, `mailbox.go` — 1.38k, `run_queue_pump.go` — 928. Один invariant одновременно размазан между agent, coordinator, mailbox, session service, SQL и app adapter.
- Исторические комментарии часто длиннее текущего контракта и уже расходятся с ним. Например, `DeleteInterruptInject` документирован как delete после успешного OS-lock acquisition, но реальный consume удаляет row до построения call, а `startDetachedRun` удаляет её ещё до enqueue.
- В коде остаются несколько независимых fire-and-forget механизмов именно на границах persistence/shutdown: checkpoint, lock metadata cleanup, queued summary handoff, interrupt ticker.
- Durable queue обещает idempotency/exactly-once вокруг неидемпотентных LLM/tool side effects, но не несёт end-to-end execution/fencing ID во всех слоях.
- Tests в основном проверяют helper happy paths. Не хватает составных сценариев через JSON round-trip, mailbox replacement, canceled context, late DB writer и config credential rotation.

Рекомендуемая архитектурная граница: один session executor с формальной state machine (`accepted -> durable -> leased(owner,generation) -> running -> committed/failed`) и единым lifecycle registry. Mailbox должен отвечать только за ordering/wakeup, durable store — за ownership/retry, а message writes — принимать fencing/sequence. Сейчас эти обязанности перекрываются, поэтому локальное исправление одного окна регулярно создаёт другое.

## Рекомендуемый порядок исправлений

1. Сохранить `LogicalCallID`/durable row ID через сериализацию и запретить re-enqueue durable-originated orphan как новую row; закрыть P0-1 end-to-end тестом.
2. Переделать interrupt handoff на claim/state transition без destructive consume до durable/mailbox commit; recovery выполнять отдельным bounded context.
3. Убрать runnerless orphan fallback и сделать finalizer failure частью terminal outcome.
4. Сделать checkpoint writes cancelable, joinable и version-fenced; учитывать timeout как forced shutdown.
5. Ограничить весь `RunQueuePump.Stop`, включая main `run/tick`, одним deadline.
6. Fail-closed прекращать execution при невозможности подтвердить lease до safety deadline; зафиксировать at-least-once семантику либо добавить fencing.
7. Исправить model cache invalidation/full fingerprint и пересборку pinned call после credential refresh; убрать shared-model fallback.
8. Удалить prompt/raw argv из INFO logs.
9. После P0/P1 — stabilization freeze без новых features и один интеграционный release gate на overlap/crash/restart/shutdown/config-rotation сценарии.

## Минимальный release gate

До stable release должны одновременно выполняться следующие свойства:

1. Durable call после JSON round-trip, replacement и error handoff существует в одной row и вызывает provider не более одного раза.
2. После успешного consume cross-process interrupt всегда находится либо в mailbox живого owner, либо в durable queue; fault в любом промежуточном шаге оставляет retryable marker.
3. Ошибка orphan enqueue не оставляет единственную копию call в idle mailbox.
4. После graceful `CancelAll`/`Shutdown` нет ни одного checkpoint/title/summary/pump writer с правом записи в session DB.
5. Старый checkpoint snapshot не может заменить более новый terminal message независимо от порядка завершения DB calls.
6. `RunQueuePump.Stop` возвращается за ограниченное время даже при зависшем main tick и сообщает forced state.
7. После expiry/rotation credentials следующий retry строит новый provider client; model cache не возвращает старый token/options.
8. Логи уровня INFO не содержат prompt, message history, MCP token или полный argv с пользовательскими данными.

Пока эти свойства не выполнены, маркировать `684e79fb` как стабильный release не следует.
