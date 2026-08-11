# Статический review готовности релиза после multi-agent fixes

Дата: 2026-08-11  
Режим: только чтение production-кода и истории Git; тесты, сборка, lint и race detector не запускались.  
Точка ревью: `78f2090c9f9eb63560308cd6eb0247b4a9471e39` (`main`).  
База сравнения: `33a696c7` — последний коммит перед предыдущим follow-up review.  
Диапазон: `33a696c7..78f2090c`, 40 коммитов, 104 файла, `+14135/-1227`.  
Не включены в оценку незакоммиченные `CHANGELOG.md` и `dev/`.

## Итог

**Вердикт: NO-GO для стабильного релиза.**

Большая часть первоначальных freeze/deadlock причин действительно закрыта:

- `SessionLock.Release` освобождает OS lock и закрывает descriptor до потенциально
  зависающей очистки metadata и ждёт её не более 50 ms;
- рекурсивный `Run()` заменён одним turn loop под одной mailbox reservation и
  одним OS lock;
- mailbox получил epoch/state-машину, атомарные drain/release и hard-stop;
- manual summary сериализован через mailbox + OS lock и использует snapshot
  целевой сессии;
- durable queue получила owner-scoped Ack/Nack/TerminalFail, lease renewal и
  независимый pump;
- обычные `Run()` теперь входят в bounded shutdown join.

Но новая durable-queue архитектура внесла как минимум три release blocker:

1. один durable call детерминированно может исполниться дважды после попадания в
   mailbox занятой сессии;
2. принятый idle interrupt/orphan всё ещё может потеряться до durable commit;
3. shutdown не join-ит `executeEntry` и manual-summary goroutines, поэтому может
   отменить их финальный Ack/Nack или закрыть БД под ещё живой работой.

Это не абстрактные «может быть когда-нибудь» замечания: ниже приведены
конкретные последовательности из текущего control flow. Я не нашёл нового
очевидного цикла `mutex A -> mutex B -> mutex A` в основном mailbox path, однако
гарантии «accepted exactly once», «shutdown без живых DB writers» и «никакая
сессия не останется без runner» пока не выполняются.

## Release blockers

### P0-1. Durable call дублируется после handoff в mailbox занятой сессии

Цепочка:

1. Pump арендует durable row и вызывает adapter
   (`internal/session/run_queue_pump.go:557-558`).
2. `sessionAgent.Run` видит живого in-process owner, добавляет call в
   `mailbox.submitted` и возвращает `(nil, nil)`
   (`internal/agent/agent.go:1240-1247`, `internal/agent/mailbox.go:342-371`).
3. Adapter превращает этот результат в `ErrCallQueuedNotExecuted`
   (`internal/app/app.go:71-97`).
4. Pump делает no-penalty Nack, то есть возвращает исходную durable row в
   `pending`, и ставит только локальный backoff на один TTL
   (`internal/session/run_queue_pump.go:584-625`).
5. Живой owner независимо извлекает mailbox-копию и исполняет её. В
   `SessionAgentCall`/`SessionAgentCallData` нет ID durable row или callback,
   которым owner мог бы подтвердить её выполнение
   (`internal/agent/agent.go:275-310`, `internal/session/session.go:1336-1375`).
6. После backoff исходная row всё ещё `pending`; pump снова вызывает `Run()` и
   исполняет тот же логический запрос второй раз.

Для обычного call с пустым `ExistingMessageID` второй запуск создаст второе user
message (`internal/agent/agent.go:1669-1693`). Для interrupt-inject с уже
существующим message ID он может создать второй assistant response на один user
message. `ErrCallAlreadyAttempted` это не предотвращает: маркер появляется при
ошибке после persistent trace, а здесь первая mailbox-копия может успешно
завершиться.

Тест `TestReleaseGate_P350_QueuedNotExecutedBacksOffWithoutAttemptPenalty`
проверяет, что row переживает много backoff-циклов и затем успешно запускается
повторно (`internal/session/p350_dup_dispatch_test.go:423-520`). Он не моделирует
исполнение уже добавленной mailbox-копии и фактически закрепляет повторный вызов
координатора как желаемое поведение.

Что исправить: не копировать durable call в mailbox без протокола подтверждения.
Подходящие варианты:

- передавать `run_queue_entry_id` в mailbox и Ack делать owner-ом только после
  фактического исполнения;
- либо добавить `TryRunIfIdle`, который при занятой сессии возвращает `busy`, но
  ничего не добавляет в mailbox;
- либо сделать durable queue единственным источником queued work, а mailbox —
  только механизмом wake-up/ownership.

Нужен end-to-end тест: active owner A, durable B, B попадает в busy path, A
реально drain-ит B, проходит больше TTL/backoff, а число user turns/provider
calls для B остаётся ровно 1 и durable row исчезает.

### P0-2. «Принятый» вызов всё ещё может потеряться до durable commit

Есть два независимых пути.

#### Idle `InterruptAndSend`

`InterruptAndSend` при отсутствии owner вызывает `startDetachedRun`, после чего
безусловно возвращает `nil` (`internal/agent/coordinator.go:2221-2252`). Но
`startDetachedRun` не возвращает ошибку. Если JSON marshal или
`EnqueueRunQueueEntry` не удались, функция только пишет log и возвращается
(`internal/agent/coordinator.go:2288-2340`). Для обычного web interrupt
`InjectID == ""`, поэтому даже fallback `pending_injects` отсутствует.

Особенно опасен caller context: enqueue намеренно использует исходный `ctx`.
Отмена HTTP request/client disconnect может отменить запись, но API уже не
способен сообщить caller-у failure. Для cross-process inject тот же отменённый
context используется при попытке воссоздать row, поэтому и fallback может не
состояться.

#### Orphaned mailbox work

`restartOrphanedWithRetry` запускает отдельную goroutine на каждый call и сразу
возвращает (`internal/agent/agent.go:1095-1127`). Комментарий утверждает
«durably enqueue BEFORE returning control», но durable commit происходит уже
после возврата caller-а. Между spawn и commit процесс может завершиться. При
ошибке enqueue call кладётся в idle mailbox через `queue()`, однако runner для
такой очереди не запускается; сам log прямо признаёт data-loss risk
(`internal/agent/agent.go:1112-1120`).

Что исправить:

- сделать enqueue синхронным и возвращающим ошибку до подтверждения запроса;
- использовать отдельный bounded `WithoutCancel` context там, где запрос уже
  принят системой;
- не использовать runnerless in-memory fallback;
- генерировать logical call ID до первой попытки и сохранять один и тот же ID во
  всех retry/fallback путях.

### P0-3. `RunQueuePump.Stop` не ждёт выполняющиеся entries и отменяет их DB outcome

`RunQueuePump.wg` учитывает только главный `run()` loop
(`internal/session/run_queue_pump.go:140-147,251-284`). `processEntry` запускает
`go p.executeEntry(...)` без `wg.Add` (`internal/session/run_queue_pump.go:464-469`).
Поэтому `Stop()` может вернуться, пока worker всё ещё выполняет call или его
Ack/Nack/TerminalFail.

Дополнительно:

- coordinator вызывается с `context.Background()`, не связанным с lifecycle
  pump (`internal/session/run_queue_pump.go:480-481,557-558`);
- lease renewal и все outcome-записи используют `p.ctx`;
- `Stop()` сначала отменяет именно этот context
  (`internal/session/run_queue_pump.go:263-272`).

Значит worker может успешно закончить `Coordinator.Run`, после чего получить
`context canceled` на Ack. Код всё равно пишет «executed entry successfully»
даже при ошибке Ack (`internal/session/run_queue_pump.go:574-581`). Row останется
leased/pending и после рестарта выполнится повторно.

В `App.Shutdown` это реальная гонка: `CancelAll` ждёт до возврата `Run()`, но
`executeEntry` заканчивает renewal и Ack уже **после** возврата `Run()`. Затем
cleanup вызывает `pump.Stop`, считает pump остановленным и graceful branch
закрывает БД (`internal/app/app.go:1911-2012`). Узкое окно между `Run` return и
Ack ничем не join-ится.

Что исправить: отдельный worker `WaitGroup`/`errgroup`, запрет новых lease после
начала stop, согласованная политика drain-or-cancel, а для финального outcome —
bounded context, который не отменяется раньше durable Ack/Nack. `Stop()` должен
возвращаться только после всех workers или явно сообщать forced state, который
запрещает закрывать БД.

### P0-4. Shutdown join не охватывает manual summary и другие detached writers

`CancelAll` ждёт только `sessionAgent.runWg`, а в неё входят лишь вызовы
`Run()` (`internal/agent/agent.go:1206-1212,4325-4384`). Public `Summarize()`
берёт mailbox ownership, запускает provider stream и пишет summary/messages, но
не входит в эту wait group (`internal/agent/agent.go:3046-3062,3095-3276`).

`hardStop` действительно вызовет cancel manual compaction, но `CancelAll` может
сразу вернуть `stillBusy=false`, не дожидаясь unwind, lock release и последних
DB операций. После этого graceful shutdown закроет БД. С uncooperative provider
summary может продолжить жить существенно дольше. Аналогичный меньший хвост —
abandoned title-generation goroutine: bounded join разрешает `Run()` вернуться,
а поздний completion всё ещё может делать detached `Rename`
(`internal/agent/agent.go:1726-1779`).

Что исправить: единый lifecycle registry для всех session-scoped execution
units (`Run`, manual summary, pump worker, title task с правом DB write).
`stillBusy=false` допустим только после их полного join.

## Высокий приоритет

### P1-1. `WaitGroup.Add` может гоняться с `CancelAll.Wait`

`Run()` делает `runWg.Add(1)` **до** проверки `shuttingDown`
(`internal/agent/agent.go:1206-1227`), а `CancelAll` сначала ставит latch, затем
запускает goroutine с `runWg.Wait()` (`internal/agent/agent.go:4325-4374`). Новый
`Run` от pump/server может вызвать positive `Add` одновременно с `Wait` на
нулевом counter. Это запрещённый контракт `sync.WaitGroup` и может привести к
panic `Add called concurrently with Wait` либо к тому, что shutdown не увидит
этот короткий execution unit.

Нужен общий admission mutex/state gate: проверка shutdown и регистрация work
должны быть одной атомарной операцией относительно начала join.

### P1-2. Потеря lease не останавливает неидемпотентное LLM execution

Renewal loop при `ok == false` только логирует потерю lease и выходит. `execCtx`
не отменяется, `Coordinator.Run` продолжает выполнять tool/LLM side effects
(`internal/session/run_queue_pump.go:529-551`). Новый owner уже может выполнять
ту же row. Owner-scoped Ack защищает row от удаления старым owner-ом, но не
защищает от двух реальных исполнений; комментарий кода сам признаёт, что
duplicate work may follow.

Lease queue с неидемпотентным consumer не может полагаться только на fencing
Ack. Нужен fencing token, проверяемый до каждого persistent side effect, либо
исполнение должно останавливаться при потере ownership. Минимум — cancel
`execCtx`, запрет post-loss commits и явная terminal/reconciliation policy.

### P1-3. FIFO durable queue не определён для calls в одну секунду

`created_at` записывается через `time.Now().Unix()`
(`internal/session/session.go:1384-1399`), а oldest row выбирается только по
`ORDER BY created_at ASC` (`internal/db/sql/run_queue.sql:14-21`). Несколько
prompts одной сессии, поставленные в очередь за одну секунду, имеют одинаковый
sort key и могут выполняться в произвольном порядке. Это обычный сценарий для
parallel agents/interrupts, не редкая clock anomaly.

Нужен монотонный per-session sequence либо как минимум более точный timestamp и
детерминированный tie-breaker, семантически соответствующий порядку enqueue.

### P1-4. Pump создаёт неограниченный fan-out после backlog/restart

`ListPendingRunQueueEntries` возвращает весь backlog, а один tick запускает по
goroutine на каждую не занятую session; глобального semaphore/worker limit нет
(`internal/session/run_queue_pump.go:286-353,469`). Большой backlog после crash
может одновременно открыть сотни provider streams, DB preambles и subprocesses,
что само способно выглядеть как freeze или вызвать rate-limit/retry storm.

Нужны paginated claims и ограниченный worker pool; per-session `inFlight` следует
сохранить как дополнительное, а не единственное ограничение.

### P1-5. Async lock cleanup может стереть metadata нового живого owner

После unlock/close старая goroutine без OS lock делает `Truncate(0)` и удаляет
sidecar (`internal/session/lock.go:503-531`). Если cleanup превысил bounded 50 ms,
а новый owner уже успел acquire и записать свой PID, старая cleanup сотрёт уже
его metadata. OS-lock correctness от этого не ломается, но зависший holder станет
`unidentified-holder`, и `crush sessions kill` не сможет его завершить
(`internal/cmd/sessions_kill.go:270-300`). Для системы, где rescue command —
последний способ снять freeze, это выше косметического риска.

Дополнительно комментарии `sessions_kill.go:123-159` всё ещё говорят, что
metadata-cleanup повторно берёт OS lock, хотя текущая реализация специально
этого не делает. Это опасный documentation drift вокруг safety-critical кода.

Безопаснее хранить metadata в owner-versioned sidecar и удалять её compare-and-
delete по generation/token, не трогая общий lock-file content после unlock.

## Средний приоритет, security и code smells

### P2-1. «Idempotency key» не является ID логического call

Обычные пути каждый раз создают новый `sessionID + UnixNano`
(`internal/agent/agent.go:1099-1102`, `internal/agent/coordinator.go:2305-2309`),
хотя service contract требует сгенерировать key один раз и повторно использовать
при retry (`internal/session/session.go:1384-1386`). Это не idempotency, а
временный unique ID. На clock с грубой resolution два concurrent calls одной
сессии также могут столкнуться по primary key; второй enqueue станет ошибкой.

### P2-2. Agent recursion guard всё ещё обходится через `env -S`

`stripEnvWrapperArgs` просто пропускает `-S` и следующий token
(`internal/agent/agentguard/agentguard.go:450-481`). Но именно следующий token —
строка, которую `env -S` затем заново делит и исполняет. Команда вида
`env -S 'claude -p test'` оставляет guard без `head` для повторной проверки и
проходит. Вариант `--split-string=...` также не разбирается. Это позволяет
случайно или намеренно запустить вложенный agent CLI, умножить стоимость и
создать рекурсивные/зависшие процессы.

`CheckWindowSafety` к тому же использует старый wrapper list и не снимает
`env`/`nice`/`timeout` (`internal/agent/agentguard/agentguard.go:333-373`), поэтому
эти wrappers обходят запрет видимых окон на Windows.

### P2-3. Codex MCP token остаётся в argv/query string

Codex path строит inline config через `mcpSrv.mcpURL()` с `?token=`
(`internal/agent/cliprovider/provider.go:1008-1016`,
`internal/agent/cliprovider/mcpserver.go:88-92`). Token виден в process list/
`/proc/<pid>/cmdline` и может попасть в diagnostic logs. Qwen/Gemini уже
переведены на `Authorization: Bearer`; Codex path следует привести к тому же
уровню или передавать secret через защищённый temporary config/env mechanism.

### P2-4. SSRF policy блокирует не все non-public ranges

Guard правильно работает в dial-time `Control` hook и отключает proxy, но
allowlist-by-negation проверяет только loopback, RFC1918/ULA, link-local,
link-local multicast и unspecified (`internal/agent/tools/ssrf_guard.go:39-84`).
Остаются, например, IPv4 shared address space `100.64.0.0/10`, benchmark
`198.18.0.0/15`, прочие reserved/multicast ranges. Для defense-in-depth проще и
надёжнее разрешать только явно global-routable unicast addresses с отдельной
политикой исключений.

### P2-5. Mailbox finalizer открыто допускает reordering между ownership eras

`abandonOwnershipWithHandoff` делает `abandonOwnership` и `popAllSubmitted`
двумя разными critical sections. Между ними новый owner может начать новую era,
а старый finalizer забрать уже её queued call и отправить в durable recovery
(`internal/agent/mailbox.go:1156-1187`). Код документирует это как accepted
reordering. После появления durable queue и exactly-once проблем такое
допущение лучше убрать, а не оставлять как комментарий.

### P2-6. Recovery subsystem слишком сложен и содержит мёртвые/ложные контракты

- `run_queue_pump.go` вырос до 662 строк с несколькими локальными state maps,
  detached goroutines и длинными историческими комментариями; инварианты
  распределены между `agent`, `app`, `session`, SQL и adapter-ом;
- `RunQueuePumpConfig.DataDirectory` не используется;
- `terminal_failure` хранится в schema/domain, но terminal failure удаляет row;
- `startDetachedRun` больше ничего не «start»-ит и скрывает ошибки;
- `Stop` обещает graceful shutdown, хотя ждёт только tick loop;
- success log печатается даже после failed Ack;
- комментарии «durably enqueue before returning» противоречат goroutine spawn.

Это не только эстетика: почти каждый найденный blocker появился на границе двух
таких частичных контрактов. Нужен один session executor/queue owner с короткой,
формально записанной state machine и минимальным числом fire-and-forget путей.

## Что из предыдущего review закрыто

| Предыдущее замечание | Статус на `78f2090c` |
|---|---|
| P0-1: `Release()` навсегда держит mailbox | Закрыто по freeze-семантике: unlock/close синхронны, metadata wait bounded 50 ms. Остался metadata-clobber P1-5. |
| P0-2: нет durable runner | Механизм добавлен, но end-to-end guarantee не закрыта: P0-1/P0-2/P0-3 выше. |
| P1-1: summary читает разные snapshots | В основном закрыто immutable `SummarizeSnapshot`. |
| P1-2: summary queue/events расходятся с ownership | Существенно улучшено: success coalescing и ownership drains добавлены. Shutdown lifecycle summary остаётся P0-4. |
| P1-3: shutdown — polling, не join | Для `Run()` закрыто через `runWg`; для pump/manual summary/title — не закрыто. |
| P1-4: watchdog не hard boundary | Покрытие providers и external CLI улучшено. Абсолютная гарантия всё ещё зависит от transport/provider; lifecycle join важнее дополнительных timeout-ов. |
| P2-2: package-global mutable test seams | Закрыто options/instance seams. |
| P2-3: divergent recovery policies | Частично унифицировано durable queue, но общая сложность и divergent ownership contracts остались. |

## Общий обзор качества кода

### Сильные стороны

- Mailbox state transitions теперь значительно лучше выражают ownership, epoch и
  `mbReleasing`; большинство прежних lost-wakeup/self-lock окон адресовано.
- Fail-closed OS lock acquisition и owner-scoped queue mutations — правильные
  safety defaults.
- Summary snapshot и cancel-immune bounded commit contexts устранили несколько
  реальных cross-session/config races.
- DB pool больше не держит global mutex во время потенциально медленного Close.
- Последние security fixes закрыли wrapper permission bypass, proxy SSRF bypass,
  часть MCP races, non-atomic download и attachment collisions.
- В commit history хорошо зафиксированы причины решений и revert-check сценарии.

### Системные риски

- `agent.go` и `coordinator.go` совмещают execution, persistence, recovery,
  watchdog, ownership и UI event semantics; локальное исправление легко меняет
  скрытый контракт соседнего слоя.
- At-least-once durable queue обслуживает неидемпотентные LLM/tool side effects,
  но не имеет end-to-end execution ID/fencing/ack handoff.
- Fire-and-forget goroutines используются именно на границах durability и
  shutdown, где потеря ownership наиболее опасна.
- Большие исторические комментарии иногда заменяют executable invariant и уже
  расходятся с кодом. Их стоит сократить до текущего контракта, а историю оставить
  Git/ADR.

## Рекомендуемый порядок исправлений

1. Спроектировать один exactly-once handoff между durable row и mailbox; закрыть
   P0-1 end-to-end тестом.
2. Сделать все enqueue paths синхронно подтверждаемыми и убрать runnerless
   fallback; закрыть P0-2 fault-injection тестами на canceled ctx/DB error.
3. Ввести общий lifecycle/admission registry и join-ить pump workers, manual
   summary и поздние writers до DB close; закрыть P0-3/P0-4/P1-1.
4. Добавить lease fencing/cancel-on-loss, deterministic per-session sequence и
   bounded worker pool.
5. Убрать lock metadata clobber через generation-aware sidecar.
6. После исправлений — короткий stabilization freeze без новых features и новый
   release gate, включающий реальные overlap/crash/shutdown сценарии, а не только
   изолированные happy paths.

## Минимальный новый release gate

До stable release должны одновременно выполняться следующие свойства:

1. Durable B, попавший в mailbox занятого A, исполняется и списывается ровно один
   раз даже после нескольких TTL и рестарта процесса.
2. Idle interrupt при canceled request context либо возвращает caller-у ошибку,
   либо уже существует durable row; «nil + ничего на диске» невозможно.
3. Ошибка durable enqueue orphan-а не оставляет call только в idle mailbox.
4. `RunQueuePump.Stop` не возвращается до завершения worker outcome либо явно
   переводит shutdown в forced mode без DB close.
5. Active manual `/compact` входит в shutdown join; после graceful return нет ни
   одной goroutine с правом записи в session DB.
6. `Run` admission одновременно с `CancelAll` не вызывает WaitGroup misuse и не
   проходит мимо join.
7. Потерявший lease executor не делает дальнейших persistent side effects.
8. Два calls одной сессии, enqueue-нутые в одну секунду, всегда сохраняют порядок.
9. Backlog ограничен настроенной глобальной concurrency и не создаёт goroutine/
   provider storm.
10. Медленная cleanup старого lock owner не стирает PID/generation нового owner.

Пока эти свойства не доказаны, маркировать текущий HEAD как stable не следует.
