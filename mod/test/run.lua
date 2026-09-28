-- Offline check of PZAdminCompanion.lua against stubbed game functions.
-- Run from the repository root with Lua 5.1: lua5.1 mod/test/run.lua
-- It checks the output is JSON-shaped and that live mode follows the flag.

local files, clock = {}, 1000000

function isServer() return true end
function getTimestampMs() return clock end

local function writer(name)
    local buf = {}
    return { write = function(_, s) buf[#buf + 1] = s end,
             close = function() files[name] = table.concat(buf) end }
end
function getFileWriter(name) return writer(name) end
function getFileReader(name)
    local body = files[name]
    if not body then return nil end
    return { readLine = function() return (string.gsub(body, "\n.*", "")) end, close = function() end }
end

local function obj(methods) return methods end
function getGameTime()
    return obj({ getYear = function() return 1993 end, getMonth = function() return 6 end,
                 getDay = function() return 8 end, getHour = function() return 14 end,
                 getMinutes = function() return 5 end, getWorldAgeHours = function() return 530.25 end })
end
function getClimateManager()
    return obj({ getTemperature = function() return 21.456 end, getRainIntensity = function() return 0.4 end,
                 getSnowIntensity = function() return 0 end, getFogIntensity = function() return 0 end,
                 getWindspeedKph = function() return 12.3 end,
                 getSeason = function() return obj({ getSeasonName = function() return "Summer" end }) end })
end
local players = {
    obj({ getUsername = function() return 'bob"the\\builder' end, getDisplayName = function() return "Bob\n" end,
          getX = function() return 10890.7 end, getY = function() return 9412.2 end, getZ = function() return 0 end,
          getBodyDamage = function() return obj({ getOverallBodyHealth = function() return 87.6 end,
                                                  IsInfected = function() return false end }) end,
          isDead = function() return false end, isAsleep = function() return false end,
          getVehicle = function() return nil end, getZombieKills = function() return 42 end,
          getHoursSurvived = function() return 71.26 end,
          getDescriptor = function() return obj({ getProfession = function() return "carpenter" end }) end }),
    -- A player object missing most getters must not throw.
    obj({ getUsername = function() return "sparse" end }),
}
local list = { size = function() return #players end }
function list.get(_, i) return players[i + 1] end
function getOnlinePlayers() return list end

local ticks = {}
Events = { OnTick = { Add = function(f) ticks[#ticks + 1] = f end } }

dofile("mod/PZAdminCompanion/Contents/mods/PZAdminCompanion/42/media/lua/server/PZAdminCompanion.lua")
local C = PZAdminCompanion
assert(#ticks == 1, "OnTick handler registered")

local function tick(ms) clock = clock + ms; ticks[1]() end

-- First tick writes a slow snapshot.
tick(0)
local snap = files[C.SNAPSHOT_FILE]
assert(snap, "snapshot written")
assert(string.sub(snap, -1) == "\n", "snapshot ends with a newline")
assert(string.find(snap, '"live":false', 1, true), "starts slow")
assert(string.find(snap, '"intervalMs":60000', 1, true))
assert(string.find(snap, '"dayNumber":23', 1, true), "day number")
assert(string.find(snap, '"month":7', 1, true), "month is 1-based")
assert(string.find(snap, '"username":"bob\\"the\\\\builder"', 1, true), "strings escaped")
assert(string.find(snap, '"name":"Bob\\n"', 1, true), "newline escaped")
assert(string.find(snap, '"x":10890', 1, true), "position floored")
assert(string.find(snap, '"temperatureC":21.50', 1, true), "rounded temperature")
assert(string.find(snap, '{"username":"sparse"}', 1, true), "sparse player")

-- Nothing new within the minute.
files[C.SNAPSHOT_FILE] = nil
tick(5000)
assert(files[C.SNAPSHOT_FILE] == nil, "slow mode waits a minute")

-- PZAdmin opens the Live tab: flag 20 s ahead.
files[C.LIVE_FILE] = tostring(math.floor(clock / 1000) + 20) .. "\n"
tick(3000)
assert(files[C.SNAPSHOT_FILE] and string.find(files[C.SNAPSHOT_FILE], '"live":true', 1, true), "live after flag")
files[C.SNAPSHOT_FILE] = nil
tick(2000)
assert(files[C.SNAPSHOT_FILE], "live writes every two seconds")

-- The tab closes: the flag is not renewed and lapses.
tick(30000)
files[C.SNAPSHOT_FILE] = nil
tick(2000)
assert(files[C.SNAPSHOT_FILE] == nil, "back to slow once the flag lapses")

-- Empty containers keep their JSON type.
assert(C.encode({}) == "[]")
assert(C.encode(C.object()) == "{}")
assert(C.encode(0 / 0) == "null")

print("ok")
