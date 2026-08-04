# Повторное ревью стабильности multi-agent изменений

Дата: 2026-08-04  
Ветка: `main`  
HEAD: `1eb4aec3547ff09cc888e10188b8e4f9d8dd1175`  
Диапазон: `e9544a8f..1eb4aec3` (76 коммитов, 69 файлов,
10 818 добавлений / 473 удаления)

## Итог

**Релиз пока нельзя считать стабильным.** В текущем состоянии остаются пять
проблем класса P0, каждая из которых способна терять пользовательские сообщения,
смешивать состояние параллельных сессий или допускать двух владельцев одной
сессии.

Главная закономерность та же, что и в прошлом ревью: локальные mutex/atomic-map
исправляют отдельные data race, но жизненный цикл сессии всё ещё разбит между
независимыми состояниями — `activeRequests`, `messageQueue`, `injectQueue`,
`summarizeQueue`, OS lock и глобально изменяемая модель coordinator. Между ними
нет одной атомарной операции передачи владения.

Рекомендуемый release decision: **NO-GO до закрытия P0-1…P0-5 и
регрессионных сценариев из раздела «Минимальный release gate».**

## Методика и ограничения

- Проведён статический review кода и Git-истории.
- Сопоставлены исправления с предыдущим отчётом
  `docs/reviews/2026-08-01-multi-agent-stability-review.md`.
- Тесты, сборка, линтеры и исполняемые команды Crush не запускались по прямому
  указанию.
- Единственное изменение рабочего дерева в рамках review — этот Markdown-файл.
- Уже существовавшее удаление `web/dist/.gitkeep` не трогалось.

## Что из прошлого review действительно исправлено

Следующие изменения выглядят корректными при статической проверке:

1. `9a9e919f` заменил рекурсивный `Run()` на turn-loop и удерживает одну
   reservation/OS-lock на всю цепочку queued turns.
2. `2ac5b335` ограничил title generation, включая fallback rename.
3. `6457e4d9` отделил перенос стоимости child-session от отменённого parent
   context.
4. `34f1dc47` сделал `--timeout-hard-cap` безусловным, в том числе во время
   tool execution.
5. `5f3db83a` и последующие изменения очищают PID metadata при нормальном
   `SessionLock.Release` и проверяют реальный OS lock перед доверием stale PID.
6. Серия `6e7e1dc6` / `cd9d505a` / `6a1e4050` / `eaf22ece` исправила
   partial-read и idle-timeout stdin.
7. Неидентифицированная ошибка OS-lock acquisition теперь приводит к fail
   closed, а не к запуску без межпроцессной защиты.

Эти исправления полезны, но не закрывают найденные ниже границы состояний.

---

## P0 — release blockers

### P0-1. Параллельные сессии могут подменять друг другу модель и provider state

**Где:**

- `internal/agent/coordinator.go:497-541` — `applyModelOverrides` изменяет общий
  `currentAgent` через `SetModels`, `SetSystemPromptPrefix` и `SetSystemPrompt`.
- `internal/agent/coordinator.go:618-655` — `runInternal` позднее заново читает
  `currentAgent.Model()` и на его основе строит provider options.
- `internal/agent/coordinator.go:927-950` — `RunWithOverrides` не удерживает
  никакой логической reservation между применением override и запуском.
- `internal/agent/agent.go:1003-1005` — `runTurn` ещё раз отдельно snapshot-ит
  модель, system prompt и prefix.
- `internal/agent/agent.go:3053-3055` — title generation снова читает текущие
  глобальные model fields.

**Сценарий:**

1. Session A применяет override `model-A`.
2. До вызова `runInternal` session B применяет `model-B` в тот же
   `currentAgent`.
3. A читает уже `model-B`, либо успевает построить options для A, но `runTurn`
   snapshot-ит B.

В результате возможна не только неправильная модель, но и несовместимая смесь:
provider options от одной модели, fantasy agent от другой, prefix/system prompt
от третьего момента времени. Atomic containers устраняют Go data race, но не
устраняют эту логическую гонку.

**Почему P0:** это прямое нарушение изоляции multi-session. Запрос может уйти к
другому provider/model, получить другой reasoning policy и другую стоимость.

**Что исправить:**

- Полностью резолвить `largeModel`, `smallModel`, provider options, prompt
  prefix и tools в immutable per-call snapshot.
- Передавать snapshot в `SessionAgentCall`/`runTurn`; не менять общий агент для
  per-session override.
- Альтернатива — отдельный agent instance на session, но нельзя лечить это
  глобальным mutex вокруг всего `Run`: это уничтожит требуемую параллельность.
- Добавить детерминированный тест с двумя одновременно остановленными в seams
  `RunWithOverrides`, проверяющий модель, provider options и system prefix в
  каждом реальном `runTurn`.

### P0-2. `InterruptAndSend` сам удаляет сообщение, которое должен запустить

**Где:**

- `internal/agent/coordinator.go:2035-2055` — сначала `QueueMessage(call)`, затем
  `Cancel(sessionID)`.
- `internal/agent/agent.go:3284-3305` — реальный `Cancel` после вызова cancel-fn
  делает `messageQueue.Clear(sessionID)`.
- `internal/agent/coordinator.go:2006-2025` — cross-process interrupt inject
  использует ту же последовательность queue-then-cancel.
- `internal/agent/interrupt_test.go:36-72` — тест проверяет только порядок
  вызовов на mock, чей `Cancel` очередь не очищает.

**Фактический поток:**

```text
QueueMessage(replacement)
  -> messageQueue содержит replacement
Cancel(sessionID)
  -> messageQueue.Clear(sessionID)
runTurn cancel-drain
  -> очередь пуста
```

Это не узкая timing race: replacement удаляется обычным production path.
`runTurn` содержит правильный по замыслу cancel-drain (`agent.go:2290-2303`),
но до него уже нечего drain-ить.

**Почему P0:** кнопка «interrupt and send» и `sessions inject --interrupt`
теряют новый пользовательский запрос.

**Что исправить:**

- Разделить семантики «обычный Cancel с очисткой очереди» и «cancel текущего
  turn с сохранением/установкой replacement».
- Предпочтительно сделать одну атомарную операцию уровня `sessionAgent`, которая
  записывает replacement и отменяет именно текущую generation.
- Интеграционный тест должен использовать реальный `sessionAgent.Cancel`, а не
  mock, и доказать, что replacement дошёл до следующего turn.

### P0-3. Потерянное пробуждение между финальным drain и release reservation

**Где:**

- `internal/agent/agent.go:654-690` — reservation защищена `sessionStartMu`.
- `internal/agent/agent.go:765-775` — busy caller отдельно добавляет сообщение
  в `messageQueue`.
- `internal/agent/agent.go:2348-2353` — владелец отдельно делает последний
  `PopFront`.
- Deferred `releaseSessionReservation` удаляет busy state уже после возврата из
  `runTurn`.

**Сценарий:**

1. Владелец делает финальный `PopFront` и видит пустую очередь.
2. Новый caller до `activeRequests.Del` видит session busy, добавляет prompt в
   очередь и возвращает `nil`.
3. Старый владелец удаляет reservation и завершает `Run`.
4. Сообщение остаётся в очереди без runner до следующего внешнего send.

`csync.KeyedQueue` делает отдельные `Append`/`PopFront` атомарными, но не делает
атомарной передачу между queue и `activeRequests`. Существующие queue-drain
тесты кладут сообщения во время preamble/stream, а не в окно последнего
`PopFront -> reservation release`.

**Что исправить:**

- Объединить «queue-or-become-owner» и «drain-or-release-owner» под одной
  per-session state machine / одним mutex.
- Reservation можно освобождать только после атомарной проверки mailbox.
- Добавить seam непосредственно перед финальным handoff и детерминированный
  тест concurrent send в этом окне.

### P0-4. Summarization не входит в единое владение сессией и может удалять
живую историю

**Где:**

- `internal/agent/agent.go:2360-2365` — `Summarize` делает неатомарный
  `IsSessionBusy`, затем запускает standalone compaction.
- `internal/agent/agent.go:2404-2423` — комментарий прямо разрешает
  `runSummarizeCore` работать параллельно с обычным `Run`.
- `internal/agent/agent.go:2441-2454` и `2584-2593` — manual compact snapshot-ит
  и затем удаляет все non-pinned сообщения snapshot-а.
- `internal/agent/agent.go:1587-1603` — background silent summary запускается
  отдельной goroutine; флаг `bgSummarizeLaunched` действует только в пределах
  одного `runTurn`.
- `internal/agent/agent.go:2458-2462` и `2659-2663` — concurrent summaries
  просто перезаписывают один cancel-fn под ключом `sessionID+"-summarize"`;
  первый defer способен удалить регистрацию второго.

**Сценарии:**

- Manual summary видит session idle; одновременно новый `Run` получает
  reservation. Summary и Run теперь пишут/удаляют одну историю параллельно.
- Следующий queued turn запускает второй background summary, пока summary из
  прошлого turn ещё работает. Оба создают summary message, удаляют
  пересекающиеся строки и соревнуются за `SummaryMessageID`.
- Manual и silent summary также не имеют атомарного взаимного исключения.

**Почему P0:** это риск удаления ещё используемой истории, неверного summary
pointer и необратимого логического повреждения контекста сессии.

**Что исправить:**

- Сделать summarization состоянием того же per-session owner/mailbox, что и
  обычный turn; check-and-reserve должен быть атомарным.
- Не разрешать две compaction одного session одновременно.
- Не удалять snapshot истории конкурентно с активным turn. Background summary
  может готовить текст, но commit summary pointer + delete должен происходить
  при подтверждённой generation/revision (transaction/CAS) или через owner.
- Не хранить cancel ownership в перезаписываемом synthetic string key.

### P0-5. Cleanup-команды всё ещё могут unlink-нуть живой OS lock и создать
двух владельцев

**Где:**

- `internal/cmd/sessions.go:1008-1037` уже документирует accepted TOCTOU:
  probe отпускает OS lock до последующего `os.Remove`.
- `internal/cmd/sessions.go:1134-1152` — обычный `sessions locks` по умолчанию
  автоматически удаляет старый path после отдельного probe.
- `internal/cmd/sessions_reap.go:81-147` — решение основано на PID/mtime, а
  удаление выполняется позже без владения реальным lock; `--all` удаляет и
  unreadable metadata.
- `internal/cmd/sessions_kill.go:93-105` — lock удаляется даже если kill не
  удался или PID остался жив.
- `internal/cmd/sessions_kill.go:159-187` — неизвестная ошибка lock probe
  приводит к kill сохранённого PID вместо fail closed.
- `internal/cmd/sessions.go:386-405` — `sessions reset --force` игнорирует
  результат kill, удаляет lock path и затем удаляет session messages.

На POSIX удаление path не снимает `flock` со старого inode. Старый процесс
продолжает считать себя владельцем, а новый процесс создаёт новый inode с тем
же именем и тоже успешно получает lock. То есть rescue/diagnostic command сама
разрушает основной invariant «один session ID — один владелец».

**Что исправить:**

- Вообще не удалять стабильные per-session lock-файлы в автоматическом cleanup:
  пустой persistent file без OS lock безопасен и уже поддержан нормальным
  `Release`.
- `sessions locks` должен быть read-only по умолчанию.
- Kill/reset должны возвращать структурированный результат, fail closed на
  неизвестной ошибке probe и продолжать только после смерти процесса и
  успешного получения реального OS lock.
- `reset --force` должен удерживать полученный OS lock до завершения DB reset.
- Не считать PID/mtime достаточным основанием для unlink.

---

## P1 — исправить до release candidate

### P1-1. `InjectMessage` дублирует или запаздывает на границах turn

**Где:** `internal/agent/agent.go:3319-3332`, drain в
`internal/agent/agent.go:1523-1530`.

Метод сначала сохраняет message в DB, затем отдельно проверяет
`IsSessionBusy`, затем добавляет ту же строку в `injectQueue`.

- Если `Run` стартовал после DB Create и уже прочитал эту строку в history,
  последующий append в `injectQueue` вставит её в prompt второй раз.
- Если inject попал после последнего `PrepareStep`, но до release reservation,
  текущий turn его не увидит; stale queue entry останется. Следующий Run уже
  прочитает строку из DB и дополнительно drain-ит ту же строку из queue.

Рекомендуется использовать один generation-aware mailbox, лучше durable
`pending_injects`, с атомарной отметкой, какой turn/generation обязан его
потребить. Финальный handoff должен учитывать inject mailbox наравне с prompts.

### P1-2. После исправления P0-2 interrupt всё равно потеряется во время DB
preamble

**Где:**

- `internal/agent/agent.go:755-763` — один `runCtx` охватывает весь loop.
- `internal/agent/agent.go:850-870` — во время preamble в activeRequests лежит
  `runCancel`.
- `internal/agent/agent.go:1043-1085` — canceled preamble сразу возвращает
  `hasNext=false`, не проверяя message queue.

Если replacement сохранить, а затем interrupt отменит `runCtx` во время
preamble, outer context становится необратимо canceled. `runTurn` выйдет из
preamble, а `Run` завершится без drain replacement. Существующий
`run_cancel_between_turns_test.go` доказывает только, что cancel прерывает
preamble; queue+cancel вместе он не проверяет.

Нужен durable dispatcher context и отдельная cancelable generation для каждого
turn/preamble. Отмена generation должна возвращать управление dispatcher-у,
который атомарно решает: взять replacement или завершить ownership.

### P1-3. Persistent task queue даёт ложный success и навсегда оставляет
`running` tasks

**Где:**

- `internal/cmd/queue.go:404-413` — JSON parse error возвращает `nil` error и
  нулевые метрики; caller помечает task `done`.
- `internal/cmd/queue.go:257-270` — ошибки обоих `UpdateStatus` игнорируются.
- `internal/queue/queue.go:116-170` — claim переводит task в `running`, но нет
  lease/runner ID/reclaim механизма после crash или cancel процесса.

Следствия: повреждённый/пустой stdout child process выглядит успешным; DB error
на final status оставляет task `running`; crash queue runner делает claimed
tasks недоступными для будущих `queue run` навсегда.

Исправление: parse error должен быть failure с ограниченным excerpt stdout;
status-update error должен всплывать; running state требует lease + heartbeat и
reclaim либо явного восстановления unfinished claims при старте.

### P1-4. Tool timeout не соответствует конфигу и не решает late child wedge

**Где:**

- `internal/agent/agent.go:91-177` — default фактически 45 минут плюс 90 секунд
  grace для любого top-level tool.
- `internal/agent/agent.go:567-603` — grace применяется ко всем top-level tools,
  а не только delegation; нулём её отключить нельзя.
- `internal/agent/stream_watchdog.go:297-314` — fire происходит на
  `toolMaxDuration + toolCleanupGrace`.
- `internal/config/config.go:408-417` — документация всё ещё обещает default
  900s/15m и fire «past this cap».

Поэтому явный `stream_tool_timeout_seconds=5` у top-level процесса означает
фактические 95 секунд. При этом 90s grace помогает только child, который завис
почти сразу: если child работал несколько минут и затем завис в собственном
tool, parent clock всё равно сработает раньше child и обрежет его cleanup/dump.
Это прямо признано в комментарии `agent.go:137-162`.

Исправление должно различать delegation и обычный tool. Явный операторский cap
должен соблюдаться буквально для primitive tools; parent delegation должен
получать child progress/heartbeat либо отдельный delegation lifecycle, а не
скрытую uniform прибавку.

---

## P2 — не блокирует hotfix, но исправить перед стабильным релизом

### P2-1. Вложения с одинаковым именем в одну секунду перезаписываются

`internal/server/handlers.go:163-182` строит имя только из timestamp с
точностью до секунды и `filepath.Base(fileName)`, затем вызывает `os.WriteFile`.
Параллельные загрузки `report.txt` получают одинаковый path; вторая молча
заменяет содержимое первой. Использовать `CreateTemp`/`O_EXCL`, UUID или
монотонный suffix.

### P2-2. Одновременные watchdog dumps одного процесса имеют одинаковое имя

`internal/log/goroutine_dump.go:105-122` использует PID и timestamp с точностью
до секунды. Несколько зависших sub-agent turns в одном процессе могут писать в
один path и перезаписать/смешать единственные диагностические dumps. Нужен
`CreateTemp`/`O_EXCL` или уникальный sequence/UUID.

### P2-3. Hard-cap log передаёт idle duration вместо wall-clock elapsed

В `internal/agent/stream_watchdog.go:366-370` hard cap вне tool вызывает
`onFire(idle, causeHardCap)`, тогда как tool branch корректно передаёт
`now.Sub(startTime)`. Поле `elapsed` может показывать почти ноль в момент
многочасового hard-cap. На cancellation решение это не влияет, но вводит в
заблуждение при postmortem.

---

## Рекомендуемая последовательность исправлений

1. Ввести единый per-session owner/mailbox с атомарными операциями:
   `submit`, `interrupt-and-replace`, `inject`, `compact`, `drain-or-release`.
2. Перенести model/provider/prompt snapshot из mutable coordinator state в
   immutable per-call state.
3. Включить manual/silent summarization в тот же ownership protocol; добавить
   revision/CAS для commit compaction.
4. Удалить unlink lock paths из обычного cleanup и сделать kill/reset fail
   closed с подтверждённым эксклюзивным lock.
5. Добавить lease/recovery в persistent queue и перестать глотать parse/DB
   errors.
6. Разделить timeout primitive tool и delegation; синхронизировать schema/docs.
7. Исправить коллизии имён attachments/dumps и метрики watchdog.

Важно не чинить P0-2, P0-3, P1-1 и P1-2 четырьмя локальными `if`: это разные
проявления одной отсутствующей атомарной передачи владения. Иначе очередной fix
с высокой вероятностью снова откроет окно в соседней ветке.

## Минимальный release gate после исправлений

Тесты не запускались в рамках этого review; ниже перечень обязательных
регрессий для следующего этапа.

1. Две параллельные sessions с разными provider/model/reasoning prefix не
   смешивают ни один snapshot.
2. Реальный `InterruptAndSend` сохраняет replacement и исполняет его следующим
   turn: во время stream, первого preamble, preamble между turns и финального
   handoff.
3. Обычный concurrent send точно в окне `final drain -> release` либо становится
   owner, либо гарантированно исполняется старым owner.
4. Inject во время start, PrepareStep и finish попадает в prompt ровно один раз.
5. Manual compact одновременно с new Run не может стартовать; две silent/manual
   compaction одной session не пересекаются; живые сообщения не удаляются.
6. `sessions locks` не изменяет lock directory. Failed kill не удаляет path и
   не позволяет второму owner. `reset --force` не трогает DB без полученного и
   удерживаемого OS lock.
7. Queue runner после malformed JSON отмечает task failed; после simulated crash
   lease возвращает task в исполнимое состояние; final-status DB error видим.
8. Два attachment uploads и два watchdog fires в одну секунду создают четыре
   разных файла.

## Финальная оценка

Последние 76 коммитов закрыли значительную часть предыдущего списка и улучшили
диагностику, но серия follow-up fixes преждевременно объявлена сходящейся. В
текущем HEAD всё ещё есть простые, статически доказуемые пути потери prompt и
cross-session contamination. Стабильный релиз разумно делать только после
структурного объединения session ownership, а не после очередной серии
точечных timeout/lock/queue patches.
