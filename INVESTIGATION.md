# UI Truncation Investigation

## Problem

Russian text appears cut off, e.g. "(ключ содержит всю информацию" when the complete message should continue further.

## Root Cause Found

**Pubsub events are being dropped due to channel overflow.**

### Evidence from Logs

1. **AppendContent** receives all deltas correctly:
   - Final delta: `new_len: 1618, last_50: "юч содержит всю информацию)"`
   - Message is complete in the agent layer

2. **renderMessageContent** sees incomplete content:
   - Final render: `content_len: 1617, is_finished: false`
   - Missing the final character and `is_finished: true` state

3. **Pubsub events dropped** (hundreds of occurrences):
   ```
   "pubsub: event dropped, subscriber channel full","event_type":"updated"
   ```

### The Bug

- Each text delta triggers `messages.Update()` which publishes a pubsub event
- Pubsub channel buffer = 64 (`const bufferSize = 64` in broker.go)
- UI renders slower than deltas arrive
- When channel is full, events are silently dropped (non-blocking send)
- The final event (with complete content and `is_finished: true`) gets dropped
- UI displays stale/incomplete content

## Solution Implemented

### Debounced Publishing with Guaranteed Final Delivery

1. **Split `Update()` into `Save()` + `PublishUpdate()`** in `internal/message/message.go`:
   - `Save()` - saves to DB only, no pubsub event
   - `PublishUpdate()` - publishes pubsub event only
   - `Update()` - calls both (backwards compatible)

2. **Debounce logic in `internal/agent/agent.go`**:
   - Background goroutine flushes pending updates every 1 second
   - `OnTextDelta` and `OnReasoningDelta` use debounced publishing
   - Important events (`OnReasoningStart`, `OnReasoningEnd`, `OnToolInputStart`, `OnToolCall`, `OnStepFinish`) force immediate publish
   - On shutdown, goroutine publishes any pending message

### Files Changed

- `internal/message/message.go` - Added `Save()` and `PublishUpdate()` methods
- `internal/agent/agent.go` - Added debounce logic with 1 second interval

### How It Works

```
Text delta arrives
       ↓
debouncedSave(ctx, msg, false)  // false = don't force publish
       ↓
Save to DB immediately (data is safe)
       ↓
Store in pendingMessage
       ↓
If last publish was >1s ago OR force=true:
    PublishUpdate() immediately
Else:
    Wait for background ticker
       ↓
Background goroutine (every 1s):
    If pendingMessage exists AND lastPublish >1s ago:
        PublishUpdate()
       ↓
On shutdown:
    Publish any pending message
```

## Testing

1. Build: `cd D:/dev/go/crush/c && go build ./...`
2. Run crush and send a request with streaming Russian text
3. Verify message is complete (no truncation)
4. Check logs - should see far fewer "event dropped" warnings
