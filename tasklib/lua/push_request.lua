-- push_request.lua
-- Atomically:
--   1. Check thread state exists; if not, create with status=initiated
--   2. If repo provided and gh_repo not set, HSET gh_repo
--   3. RPUSH thread:<id>:messages <user-request-json>
--   4. LPUSH requests:inbox <request-payload-json>
--   5. SET requests:inbox:pending:<thread_id> sentinel (dedup, TTL 600s)
--
-- KEYS[1] = thread:<id>:current_state
-- KEYS[2] = thread:<id>:messages
-- KEYS[3] = requests:inbox
-- KEYS[4] = requests:inbox:pending:<thread_id>
-- ARGV[1] = repo (empty string if not provided)
-- ARGV[2] = timestamp (ISO8601 UTC)
-- ARGV[3] = user-request-json-message
-- ARGV[4] = request-payload-json

local thread_state_key = KEYS[1]
local thread_msgs_key = KEYS[2]
local inbox_key = KEYS[3]
local pending_sentinel = KEYS[4]

local repo_value = ARGV[1]
local now = ARGV[2]
local msg_json = ARGV[3]
local payload_json = ARGV[4]

-- Check for duplicate pending request
if redis.call("EXISTS", pending_sentinel) == 1 then
    return "duplicate"
end

-- Create or ensure thread state
if redis.call("EXISTS", thread_state_key) == 0 then
    -- Thread doesn't exist yet — create it
    redis.call("HSET", thread_state_key, "status", "initiated", "updated_at", now)
    if repo_value ~= "" then
        redis.call("HSET", thread_state_key, "gh_repo", repo_value)
    end
else
    -- Thread exists — set repo if not already present
    if repo_value ~= "" then
        if redis.call("HEXISTS", thread_state_key, "gh_repo") == 0 then
            redis.call("HSET", thread_state_key, "gh_repo", repo_value)
        end
    end
end

-- Append user request to thread history
redis.call("RPUSH", thread_msgs_key, msg_json)

-- Push request payload to inbox
redis.call("LPUSH", inbox_key, payload_json)

-- Set duplicate prevention sentinel (10 min TTL — cleared on completion by supervisor)
redis.call("SET", pending_sentinel, "1", "EX", 600)

return "ok"
