-- PZAdmin Companion
--
-- Writes a snapshot of the world and the online players to
-- Zomboid/Lua/pzadmin_companion.json, which PZAdmin reads from the server's
-- config folder. Nothing is sent anywhere and nothing runs on players' PCs.
--
-- Once a minute by default. While PZAdmin's Live tab is open, PZAdmin keeps
-- Zomboid/Lua/pzadmin_companion_live.txt holding a Unix time a few seconds
-- ahead; until that time passes the snapshot is written every two seconds.
-- Closing the tab stops the renewals, so live mode ends on its own.
--
-- Every engine call goes through get(), which checks the method exists before
-- calling it. A pcall would keep the loop alive but Project Zomboid still logs
-- a full stack trace for every throw, which at two-second intervals floods
-- console.txt.

if not isServer() then return end

local SCHEMA = 1
local MOD_VERSION = "1.0.0"
local SNAPSHOT_FILE = "pzadmin_companion.json"
local LIVE_FILE = "pzadmin_companion_live.txt"

local SLOW_MS = 60000
local LIVE_MS = 2000
local FLAG_CHECK_MS = 3000

local lastWrite = 0
local lastFlagCheck = 0
local liveUntil = 0

-- get calls obj:name() when obj and the method both exist, else returns nil.
local function get(obj, name)
    if obj == nil then return nil end
    -- Only a truthiness test: Kahlua's Java methods are not all of Lua type
    -- "function", so checking type() would hide getters that do exist.
    local f = obj[name]
    if not f then return nil end
    return f(obj)
end

local function nowMs()
    if getTimestampMs then return getTimestampMs() end
    if getTimestamp then return getTimestamp() * 1000 end
    return 0
end

-- --- JSON ------------------------------------------------------------------

local escapes = { ['"'] = '\\"', ['\\'] = '\\\\', ['\n'] = '\\n', ['\r'] = '\\r', ['\t'] = '\\t' }

local function quote(s)
    s = tostring(s)
    s = string.gsub(s, '[%c"\\]', function(c)
        return escapes[c] or string.format("\\u%04x", string.byte(c))
    end)
    return '"' .. s .. '"'
end

local encode

-- OBJECT marks a table to be written as a JSON object even when it is empty.
local OBJECT = {}
local function object(t) return setmetatable(t or {}, OBJECT) end

local function encodeNumber(n)
    -- NaN and infinity are not JSON.
    if n ~= n or n == math.huge or n == -math.huge then return "null" end
    if n == math.floor(n) and math.abs(n) < 1e15 then return string.format("%d", n) end
    return string.format("%.2f", n)
end

-- Tables made with object() are JSON objects; any other table is an array.
encode = function(v)
    local t = type(v)
    if v == nil then return "null" end
    if t == "boolean" then return v and "true" or "false" end
    if t == "number" then return encodeNumber(v) end
    if t == "string" then return quote(v) end
    if t == "table" then
        local out = {}
        if getmetatable(v) ~= OBJECT then
            for i = 1, #v do out[i] = encode(v[i]) end
            return "[" .. table.concat(out, ",") .. "]"
        end
        local keys = {}
        for k in pairs(v) do keys[#keys + 1] = tostring(k) end
        table.sort(keys)
        for i, k in ipairs(keys) do out[i] = quote(k) .. ":" .. encode(v[k]) end
        return "{" .. table.concat(out, ",") .. "}"
    end
    return quote(tostring(v))
end

-- --- collection ------------------------------------------------------------

local function round(n, places)
    if type(n) ~= "number" then return nil end
    local m = 10 ^ (places or 0)
    return math.floor(n * m + 0.5) / m
end

local function world()
    local w = object()
    local gt = getGameTime and getGameTime() or nil
    if gt then
        w.year = get(gt, "getYear")
        local month = get(gt, "getMonth")
        w.month = month and month + 1 or nil -- the game counts months from 0
        local day = get(gt, "getDay")
        w.day = day and day + 1 or nil -- and days of the month from 0
        w.hour = get(gt, "getHour")
        w.minute = get(gt, "getMinutes")
        local age = get(gt, "getWorldAgeHours")
        if type(age) == "number" then
            w.worldAgeHours = round(age, 1)
            w.dayNumber = math.floor(age / 24) + 1
        end
    end

    local cm = getClimateManager and getClimateManager() or nil
    if cm then
        w.temperatureC = round(get(cm, "getTemperature"), 1)
        w.rain = round(get(cm, "getRainIntensity"), 2)
        w.snow = round(get(cm, "getSnowIntensity"), 2)
        w.fog = round(get(cm, "getFogIntensity"), 2)
        w.windKph = round(get(cm, "getWindspeedKph"), 1)
        local season = get(cm, "getSeason")
        local name = get(season, "getSeasonName")
        if name then w.season = tostring(name) end
    end
    return w
end

local function player(p)
    local out = object({
        username = get(p, "getUsername"),
        name = get(p, "getDisplayName"),
    })
    local x, y, z = get(p, "getX"), get(p, "getY"), get(p, "getZ")
    if type(x) == "number" and type(y) == "number" then
        out.x = math.floor(x)
        out.y = math.floor(y)
        out.z = type(z) == "number" and math.floor(z) or 0
    end

    local body = get(p, "getBodyDamage")
    if body then
        out.health = round(get(body, "getOverallBodyHealth"), 0)
        out.infected = get(body, "IsInfected")
    end
    out.dead = get(p, "isDead")
    out.asleep = get(p, "isAsleep")
    if p.getVehicle then out.inVehicle = p:getVehicle() ~= nil end
    out.kills = get(p, "getZombieKills")
    out.hoursSurvived = round(get(p, "getHoursSurvived"), 1)

    local desc = get(p, "getDescriptor")
    local profession = get(desc, "getProfession") or get(desc, "getCharacterProfession")
    if profession ~= nil then out.profession = tostring(profession) end
    return out
end

local function players()
    local out = {}
    local list = getOnlinePlayers and getOnlinePlayers() or nil
    if not list then return out end
    local n = get(list, "size") or 0
    for i = 0, n - 1 do
        local p = list:get(i)
        if p then out[#out + 1] = player(p) end
    end
    return out
end

-- --- files -----------------------------------------------------------------

local function readLiveFlag()
    local r = getFileReader(LIVE_FILE, false)
    if not r then return 0 end
    local line = r:readLine()
    r:close()
    local secs = tonumber(line or "")
    if not secs then return 0 end
    return secs * 1000
end

local function writeSnapshot(now, live)
    local snap = object({
        schema = SCHEMA,
        modVersion = MOD_VERSION,
        generatedAtMs = now,
        intervalMs = live and LIVE_MS or SLOW_MS,
        live = live,
        world = world(),
        players = players(),
    })
    local w = getFileWriter(SNAPSHOT_FILE, true, false)
    if not w then return end
    -- The closing newline tells PZAdmin the write finished: a file read
    -- halfway through a write is missing it and is retried.
    w:write(encode(snap) .. "\n")
    w:close()
end

local function onTick()
    local now = nowMs()
    if now == 0 then return end

    if now - lastFlagCheck >= FLAG_CHECK_MS then
        lastFlagCheck = now
        liveUntil = readLiveFlag()
    end

    local live = now < liveUntil
    local interval = live and LIVE_MS or SLOW_MS
    if now - lastWrite >= interval then
        lastWrite = now
        writeSnapshot(now, live)
    end
end

-- Exposed for the offline test harness in the PZAdmin repository.
PZAdminCompanion = { encode = encode, object = object, onTick = onTick, SNAPSHOT_FILE = SNAPSHOT_FILE, LIVE_FILE = LIVE_FILE }

Events.OnTick.Add(onTick)
