# Release-review: зависания сессий после mailbox-миграции

Дата: 2026-08-04  
Ветка: `main`  
HEAD: `0fdfefa04e734ba083cb5dadfe3e6cc2cd9fac74`  
Проверенный новый диапазон: `1eb4aec3547ff09cc888e10188b8e4f9d8dd1175..0fdfefa04e734ba083cb5dadfe3e6cc2cd9fac74`
(17 коммитов, 25 файлов, 5158 добавлений / 184 удаления)

## Вердикт

**NO-GO: исправлены не все release blockers.**

Обычное конкурентное добавление нового `Run` в конце turn теперь защищено одной
mailbox-транзакцией, и явного цикла захвата Go mutex в новом mailbox-коде не
обнаружено. Но в текущем состоянии остаются как минимум три подтверждённых пути,
где работа принимается, а исполняющего её runner больше нет либо shutdown запускает
новую работу вместо остановки. Для пользователя это выглядит ровно как зависшая
сессия. Дополнительно остаётся неограниченный `wg.Wait` в title generation и
блокирующий дисковый I/O под `mailbox.mu`.

Поэтому утверждать «дедлоков больше нет» нельзя. Точнее: новый mailbox не показывает
классического mutex-cycle, но система всё ещё содержит lost-wakeup/runnerless-work,
неограниченное ожидание goroutine и опасные переходы ownership.

Review выполнен статически. Тесты, сборка, линтеры и исполняемые команды Crush не
запускались по прямому указанию.

## Что действительно исправлено

1. `3646b946` закрыл исходный P0-3 для обычных параллельных `Run`: `submit` и
   `drainOrReleaseFinal` используют один `mailbox.mu`, а OS lock освобождается до
   публикации `mbIdle` (`internal/agent/mailbox.go:136-148,276-363`).
2. `23004ec3` исправил детерминированную потерю сообщения в старой последовательности
   `QueueMessage -> Cancel`: replacement сначала записывается в mailbox, а при
   полученном `context.Canceled` читается через `drainAfterCancel`
   (`internal/agent/mailbox.go:421-473`, `internal/agent/agent.go:2491-2506`).
3. `6644933c` не даёт startup recovery помечать незавершённой сессию, которой всё ещё
   владеет живой процесс.
4. Коммиты `c23ef274`, `fa2d2afd`, `8ba3d3f8`, `8c975400` исправили несколько
   регрессий самой миграции: вечный `IsBusy`, abandoned owner, порядок mailbox/OS-lock
   и stale cancel при reclaim.

Эти исправления полезны и выглядят структурно правильными в проверенных ветках, но
они покрывают не все переходы state machine.

---

## P0 — новые/оставшиеся freeze blockers

### P0-A. Late interrupt оставляет replacement без runner

**Где:**

- `internal/agent/mailbox.go:421-450` записывает interrupt payload только в
  `mb.replacement`.
- `internal/agent/mailbox.go:459-473` читает replacement только из
  `drainAfterCancel`.
- `internal/agent/mailbox.go:276-363` — обычный `drainOrReleaseFinal` проверяет
  `submitted` и legacy queue, но вообще не проверяет `replacement`.
- `internal/agent/agent.go:2491-2506` вызывает `drainAfterCancel` только когда
  `agent.Stream` вернул cancel-error.
- `internal/agent/agent.go:2563-2583` при нормальном завершении идёт в финальный
  drain, который replacement не видит.

**Проигрывающая последовательность:**

1. Turn уже фактически закончил provider stream, но mailbox пока остаётся `mbOwned`.
2. Приходит `InterruptAndReplace`: replacement записан, cancel-fn вызван, метод
   возвращает success.
3. Отмена проигрывает гонку завершению; `agent.Stream` возвращает `nil`, а не
   `context.Canceled`. Такая возможность прямо признана для аналогичной отмены в
   `internal/agent/agent.go:2257-2266`.
4. Обычный финальный drain не читает replacement, освобождает ownership и OS lock.
5. `Run`-defer либо переносит payload в legacy `messageQueue`, где нет runner, либо
   становится no-op из-за уже начавшейся следующей epoch. В обоих вариантах
   replacement не гарантированно запускается сейчас.

Интеграционный тест `internal/agent/interrupt_replace_test.go:42-143` ловит interrupt
по сигналу начала stream и тем самым гарантирует mid-stream cancel. Границу
`Stream returned -> final drain` он не проверяет.

**Что исправить:** replacement должен участвовать в каждом финальном
`next-or-release` решении, а не только в error-ветке. Приоритет
`replacement -> submitted -> legacy -> release` должен быть одной операцией state
machine. Нужен детерминированный seam после возврата `Stream`, перед проверкой error и
финальным drain.

### P0-B. Idle fallback `InterruptAndSend` ставит запрос в очередь без запуска `Run`

**Где:**

- Контракт mailbox говорит, что при `mbIdle` caller должен запустить свежий `Run`
  напрямую (`internal/agent/mailbox.go:413-429`).
- Реальный coordinator вместо этого вызывает только `QueueMessage`
  (`internal/agent/coordinator.go:2024-2028,2056-2065`).
- Web handler после этого отвечает клиенту `status=queued`
  (`internal/server/handlers.go:364-374`).

Если кнопка interrupt нажата на уже закончившейся сессии или гонка попала сразу после
release, payload оказывается в legacy queue, но владельца/dispatcher уже нет. Он
будет замечен лишь при каком-то будущем внешнем `Run`. Это детерминированная
runnerless queue, а не только теоретическая data race.

**Что исправить:** idle-ветка должна проходить через обычный путь запуска `Run` либо
mailbox API должно атомарно возвращать действие `became owner, run this call`.
Нельзя отвечать `queued`, пока нет ни owner, ни durable worker, который гарантированно
заберёт запись.

### P0-C. `CancelAll`/shutdown может запустить следующий queued turn

**Где:**

- В mailbox уже есть отдельный `dispatcherCancel`, документированный именно для
  `CancelAll/process shutdown` (`internal/agent/mailbox.go:55-62`).
- `CancelAll` его не использует: он вызывает обычный `Cancel` для каждого owner
  (`internal/agent/agent.go:3653-3685`).
- Обычный `Cancel` отменяет только текущую generation
  (`internal/agent/agent.go:3561-3599`).
- Cancel-error branch тут же забирает replacement/submitted и запускает следующую
  iteration (`internal/agent/agent.go:2491-2506`).
- Через 5 секунд `CancelAll` возвращается даже при живом owner, после чего
  `App.Shutdown` начинает закрывать остальные ресурсы
  (`internal/app/app.go:1777-1819`).

То есть shutdown при наличии очереди делает противоположное требуемому: отменяет
turn N и может начать turn N+1 на всё ещё живом `runCtx`. Повторной отмены новой
generation нет. Через 5 секунд DB/cleanup могут гоняться с продолжающимся агентом.

**Что исправить:** ввести отдельную атомарную hard-stop операцию dispatcher:

- запретить любой последующий drain/restart;
- отменить durable `dispatcherCancel`, а не только generation cancel;
- явно выбрать политику очереди при shutdown (сохранить durable или очистить);
- дождаться завершения реальных owner goroutines через join, а не опрос `IsBusy` с
  безусловным выходом через 5 секунд.

Сюда же относится уже признанный, но не исправленный UX-дефект: bare Stop/Cancel
сейчас продолжает queued message вместо остановки
(`docs/checkpoints/2026-08-04-1611.md:114-125`).

---

## P1 — дополнительные пути зависания и нарушения ownership

### P1-A. Interrupt во время DB preamble отменяет весь dispatcher

`Run` создаёт один `runCtx/runCancel` на весь turn-loop
(`internal/agent/agent.go:876-884`). Перед каждым preamble mailbox получает именно
`runCancel` (`internal/agent/agent.go:1033-1062`). Если interrupt приходит до создания
per-turn `genCtx`, `InterruptAndReplace` вызывает этот whole-dispatcher cancel.

Preamble-ошибки на `sessions.Get`, `getSessionMessages` или `createUserMessage`
возвращают `hasNext=false` (`internal/agent/agent.go:1235-1277`). Replacement затем
попадает в abandon/legacy queue без немедленного runner. Это открытая задача
`#284 / P1-2`.

Нужны durable dispatcher context и отдельная cancelable generation, включающая
preamble. Generation cancel должен возвращать управление живому dispatcher, который
атомарно выбирает replacement или завершение.

### P1-B. Title timeout не ограничивает `wg.Wait`

Title generation запускается в goroutine с context timeout, но `runTurn` делает
безусловный `defer wg.Wait()` (`internal/agent/agent.go:1309-1327`). Внутри goroutine
есть блокирующий `agent.Stream(ctx, ...)` (`internal/agent/agent.go:3384-3388`).

Если provider/transport завис вне context-aware I/O и не вернётся после cancel,
истечение двухминутного context ничего не разблокирует: `wg.Wait` остаётся
неограниченным. Комментарий `agent.go:211-224` заявляет, что timer является
backstop именно для такого случая, но реализация этого не обеспечивает.

Нужен bounded join (`done` + select по отдельному deadline) и запрет фоновой title
goroutine писать session state после передачи ownership следующему turn. Простого
`context.WithTimeout` недостаточно для кода, который может игнорировать context.

### P1-C. OS-lock release выполняет дисковый I/O под `mailbox.mu`

`drainOrReleaseFinal` удерживает `mb.mu`, пока вызывает `SessionLock.Release`
(`internal/agent/mailbox.go:268-275,352-363`). `Release` внутри делает
`Truncate`, `Seek`, `Sync`, удаление sidecar, unlock и close
(`internal/session/lock.go:263-283,320-330`). Эти операции не имеют context/timeout.

Если файловая система или антивирус/SMB зависли, под тем же mutex блокируются
`submit`, `Cancel`, `InterruptAndReplace`, `IsSessionBusy`, `IsBusy` и `CancelAll` для
сессии. Обратного порядка захвата, образующего доказанный mutex-cycle, не найдено, но
это реальный unbounded blocking point, недавно добавленный ради атомарности OS-lock
handoff.

Нужен промежуточный state вроде `mbReleasing`, который не публикует idle/owner до
результата release, но и не удерживает mailbox mutex на всём дисковом I/O. Изменять
это локальным unlock опасно: требуется формально определить поведение submit/cancel
в `mbReleasing`.

### P1-D. Timer-based pulse скрывает настоящий freeze

Во время любого tool-in-flight watchdog на каждом timer tick вызывает
`recordActivity`, даже если инструмент/подагент не сообщил никакого прогресса
(`internal/agent/stream_watchdog.go:150-165,331-340`). Поэтому lock heartbeat может
оставаться свежим у реально зависшего tool до его большого cap. В checkpoint уже
зафиксирован живой случай с 38 минутами ложной активности; задача `#286 / P0-6`
остаётся открытой.

Это не создаёт mutex-deadlock, но маскирует его, мешает recovery/оператору отличить
живую работу от зависшей и растягивает симптом до default tool cap (45 минут плюс
cleanup grace). Pulse следует обновлять только реальным progress event, а не фактом
существования goroutine/tool call.

### P1-E. `InjectMessage` всё ещё вне mailbox generation protocol

`InjectMessage` сначала создаёт DB row, потом отдельно проверяет `IsSessionBusy` и
пишет в legacy `injectQueue` (`internal/agent/agent.go:3637-3650`). Mailbox уже имеет
`inject/drainInjects`, но production path их не использует
(`internal/agent/mailbox.go:475-521`). На границах PrepareStep/finish сообщение может
быть продублировано либо отложено до будущего turn. Открытая задача `#285 / P1-1`.

---

## Остальные прежние P0, которые последние 17 коммитов не закрыли

Эти проблемы не являются прямыми mutex-deadlock, но не позволяют назвать
multi-session release стабильным.

1. **P0-1 / #265 — model/provider contamination.** `applyModelOverrides` изменяет
   общий `currentAgent` (`internal/agent/coordinator.go:497-541`), а `runInternal`,
   `runTurn` и `generateTitle` перечитывают mutable state в разные моменты
   (`coordinator.go:618-655,927-950`, `agent.go:1192-1204,3330-3332`). Параллельные
   сессии могут получить смесь model/provider/options/prompt другой сессии.
2. **P0-4 / #268 — summarization без session ownership.** `Summarize` делает
   неатомарный `IsSessionBusy -> runSummarize`
   (`internal/agent/agent.go:2590-2615`), а core явно допускает параллельность с
   обычным `Run` (`agent.go:2640-2675`) и затем удаляет исторические сообщения
   (`agent.go:2837-2845`).
3. **P0-5 / #269 — небезопасный cleanup lock path.** `sessions locks` признаёт
   TOCTOU между probe и unlink (`internal/cmd/sessions.go:1008-1037,1134-1152`);
   `sessions reap` удаляет по PID/mtime без попытки получить реальный OS lock
   (`internal/cmd/sessions_reap.go:81-147`); `sessions kill` удаляет path даже если
   kill не удался или PID остался жив (`internal/cmd/sessions_kill.go:93-105,193-222`);
   `reset --force` после этого продолжает DB reset (`internal/cmd/sessions.go:386-405`).
   Результат — возможны два owner одной session и последующая SQLite contention/
   повреждение логического состояния.

Сами checkpoint-файлы подтверждают, что исходный `NO-GO` не снимался и задачи
`#265`, `#268`, `#269`, `#286`, `#284`, `#285` остаются открытыми
(`docs/checkpoints/2026-08-04-1346.md:72-84,105-109`,
`docs/checkpoints/2026-08-04-1611.md:120-128`).

## Почему существующие тесты не доказывают стабильность

Тесты в этом review не запускались; ниже только статическая оценка покрытия.

- Mid-stream interrupt test намеренно синхронизируется на начале provider stream и
  не может попасть в late-completion boundary.
- `IsBusy` regression test проверяет idle после нормального turn, но не проверяет
  `CancelAll` при наличии replacement/submitted work.
- Mailbox unit tests проверяют `drainAfterCancel` и `drainOrReleaseFinal` отдельно,
  но не закрепляют общий invariant «любая принятая работа либо имеет owner, либо
  сама становится новым owner».
- Нет сценария idle `InterruptAndSend`, который доказывает фактический запуск
  следующего turn, а не только помещение вызова в mock queue.
- Context timeout title generation тестирует отмену cooperative provider, но
  контекст не является bounded join для некооперативного provider.

## Минимальный порядок исправлений

1. Объединить `replacement` с финальным `next-or-release`; закрыть late interrupt и
   idle fallback одним формальным mailbox invariant.
2. Реализовать hard dispatcher shutdown и использовать его из `CancelAll`; отдельно
   закрепить semantics обычного Stop/Cancel.
3. Разделить dispatcher context и per-generation context так, чтобы generation
   включала DB preamble, но её отмена не убивала очередь turns.
4. Убрать неограниченный title `wg.Wait` и дисковый I/O из-под `mailbox.mu` через
   явный releasing state.
5. Завершить mailbox migration для inject и summarization; затем удалить legacy
   `activeRequests/messageQueue/injectQueue` paths (`#281/#285/#268`).
6. Перенести model/provider/prompt/options в immutable per-call snapshot.
7. Сделать lock cleanup fail-closed и убрать timer-generated pulse.

## Обязательные regression-сценарии перед release

1. Interrupt в четырёх seams: первый preamble, mid-stream, сразу после возврата
   `Stream`, final drain/release. Replacement исполняется ровно один раз.
2. `InterruptAndSend` на idle session либо немедленно становится owner, либо
   возвращает явную ошибку; состояния «queued без runner» нет.
3. `CancelAll` при текущем turn плюс `replacement` плюс `submitted` завершает owner и
   не начинает ни одного нового provider call.
4. Второй cancel/interrupt в окне между cancel-drain и следующей generation не
   попадает в spent cancel-fn.
5. Некоперативный fake title provider не удерживает `Run` после bounded join.
6. Задержанный `SessionLock.Release` не блокирует control-plane навсегда и не
   позволяет второму owner стартовать раньше release.
7. Параллельные sessions с разными model/provider/reasoning/prefix получают
   неизменяемые независимые snapshots.
8. Manual/silent summarize, inject и normal Run под детерминированными seams не
   дублируют и не удаляют живые сообщения.

## Незакоммиченное состояние во время review

До начала review уже были `D web/dist/.gitkeep` и `?? dev/`; они не трогались. Во
время статического просмотра извне появился незакоммиченный diff в
`internal/agent/mailbox.go` и соответствующем `mailbox_lock_test.go`, очищающий
`mb.current.cancel` в двух успешных ветках `drainAfterCancel`. Он закрывает ещё одно
окно со spent cancel-fn, но не исправляет ни P0-A (normal final drain не читает
replacement), ни P0-B, ни P0-C. Production-код в рамках этого review не изменялся.

## Финальная оценка

Mailbox-миграция исправила исходный ordinary-send lost wakeup и несколько своих
регрессий, но state machine ещё не сходится во всех переходах. На текущем HEAD и с
увиденным незакоммиченным round-13 diff стабильный релиз делать рано: существуют
простые последовательности, в которых API сообщает success/queued, а следующего
runner нет, и shutdown не гарантирует остановку всех dispatcher-ов.
