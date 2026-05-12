-- cancel_request.lua
-- Atomically cancels a pending request:
--   1. Removes the request from requests:inbox (where the payload contains the thread_id)
--   2. Removes the request from requests:inbox_processing (in case it was already LMOVEd)
--   3. Sets thread:<id>:current_state status to "cancelled"
--   4. Deletes the pending sentinel
--
-- KEYS[1] = requests:inbox
-- KEYS[2] = requests:inbox_processing
-- KEYS[3] = thread:<id>:current_state
-- KEYS[4] = requests:inbox:pending:<thread_id>
-- ARGV[1] = thread_id

local inbox_key = KEYS[1]
local processing_key = KEYS[2]
local thread_state_key = KEYS[3]
local pending_sentinel = KEYS[4]

local thread_id = ARGV[1]

-- Remove from inbox (scan for entries containing this thread_id)
local inbox_items = redis.call("LRANGE", inbox_key, 0, -1)
for _, item in ipairs(inbox_items) do
    -- Payload is JSON: {"request_id":"...", "thread_id":"<id>", ...}
    -- Simple string match on thread_id within the JSON blob
    if string.find(item, thread_id) then
        redis.call("LREM", inbox_key, 0, item)
    end
end

-- Remove from processing list
local proc_items = redis.call("LRANGE", processing_key, 0, -1)
for _, item in ipairs(proc_items) do
    if string.find(item, thread_id) then
        redis.call("LREM", processing_key, 0, item)
    end
end

-- Set thread status to cancelled
if redis.call("EXISTS", thread_state_key) == 1 then
    redis.call("HSET", thread_state_key, "status", "cancelled")
end

-- Clear pending sentinel
redis.call("DEL", pending_sentinel)

return "ok"
