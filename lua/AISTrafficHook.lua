--[[
    AIS Traffic Hook for DCS World

    Runs in GameGUI context (Scripts/Hooks/) and communicates with an external
    Go application via TCP to spawn, move, and remove ships based on real-world
    AIS vessel data.

    Protocol: newline-delimited JSON over TCP on localhost:18420
]]

-- LuaSocket setup (same pattern as DCSServerBot)
package.path  = package.path .. ";.\\LuaSocket\\?.lua;"
package.cpath = package.cpath .. ";.\\LuaSocket\\?.dll;"
local socket = require("socket")
local JSON = loadfile("Scripts\\JSON.lua")()

local AIS = {}

-- ---------------------------------------------------------------------------
-- Configuration
-- ---------------------------------------------------------------------------
AIS.TCP_HOST            = "127.0.0.1"
AIS.TCP_PORT            = 18420
AIS.POLL_INTERVAL       = 0.5   -- seconds between TCP reads
AIS.RECONNECT_INTERVAL  = 5     -- seconds between reconnect attempts

-- ---------------------------------------------------------------------------
-- State
-- ---------------------------------------------------------------------------
AIS.tcp              = nil      -- TCP socket handle
AIS.connected        = false
AIS.recvBuffer       = ""       -- incomplete data from TCP reads
AIS.lastPollTime     = 0
AIS.lastReconnect    = 0
AIS.trackedGroups    = {}       -- [groupName] = true
AIS.theatre          = nil      -- current map name
AIS.unitCounter      = 0        -- monotonic counter for unique unit IDs

-- ---------------------------------------------------------------------------
-- Logging helpers
-- ---------------------------------------------------------------------------
local function logInfo(msg)
    log.write('AISTraffic', log.INFO, msg)
end

local function logError(msg)
    log.write('AISTraffic', log.ERROR, msg)
end

local function logWarning(msg)
    log.write('AISTraffic', log.WARNING, msg)
end

-- ---------------------------------------------------------------------------
-- TCP helpers
-- ---------------------------------------------------------------------------

--- Disconnect and clean up the socket.
local function tcpDisconnect()
    if AIS.tcp then
        pcall(function() AIS.tcp:close() end)
        AIS.tcp = nil
    end
    AIS.connected = false
    AIS.recvBuffer = ""
    logInfo("Disconnected from AIS backend")
end

--- Send a JSON message (appends newline).
-- @param tbl table to JSON-encode
local function tcpSend(tbl)
    if not AIS.connected or not AIS.tcp then
        return false
    end
    local payload = JSON:encode(tbl) .. "\n"
    local ok, err = AIS.tcp:send(payload)
    if not ok then
        logError("TCP send failed: " .. tostring(err))
        tcpDisconnect()
        return false
    end
    return true
end

--- Send the theatre identification message.
local function tcpSendTheatre()
    logInfo("Sending theatre: " .. tostring(AIS.theatre))
    tcpSend({ type = "theatre", theatre = AIS.theatre })
end

--- Attempt to connect to the Go backend.
-- @return boolean success
local function tcpConnect()
    local now = socket.gettime()
    if now - AIS.lastReconnect < AIS.RECONNECT_INTERVAL then
        return false
    end
    AIS.lastReconnect = now

    logInfo(string.format("Connecting to %s:%d ...", AIS.TCP_HOST, AIS.TCP_PORT))

    local tcp, err = socket.tcp()
    if not tcp then
        logError("Failed to create TCP socket: " .. tostring(err))
        return false
    end

    tcp:settimeout(2) -- short blocking timeout for the connect handshake
    local ok, cerr = tcp:connect(AIS.TCP_HOST, AIS.TCP_PORT)
    if not ok then
        logError("TCP connect failed: " .. tostring(cerr))
        tcp:close()
        return false
    end

    tcp:settimeout(0) -- non-blocking from here on
    AIS.tcp = tcp
    AIS.connected = true
    AIS.recvBuffer = ""
    logInfo("Connected to AIS backend")

    -- Detect theatre if we don't know it yet (e.g. hook loaded mid-mission)
    if not AIS.theatre then
        local tok, tresult = pcall(net.dostring_in, 'server', 'return env.mission.theatre')
        if tok and tresult and #tresult > 0 then
            AIS.theatre = tresult
            logInfo("Theatre detected on connect: " .. AIS.theatre)
        end
    end

    -- Send theatre to backend
    if AIS.theatre then
        tcpSendTheatre()
    end

    -- Detect available ship models so the backend only uses installed ones.
    local allModels = {
        -- CAP Navy
        "container_ship", "fishing_vessel", "trawler_ship", "diesel_trawler",
        "lng_tanker", "ievoli_ivory",
        "jr_more_tug", "jr_more_tug_helipad",
        "yacht_ship", "yacht_helipad",
        "akademik_cherskiy", "akademik_cherskiy_pipe_laying",
        "kimedaka_skyicher", "old_vessel",
        -- DCS base (TechWeaponPack)
        "HandyWind", "Seawise_Giant", "La_Combattante_II", "BDK-775",
        -- DCS base (SouthAtlanticAssets)
        "HarborTug", "Ship_Tilde_Supply", "CastleClass_01", "leander-gun-ariadne",
        -- Currenthill Assets Pack
        "ALBATROS", "CHAP_Project22160",
    }
    local available = {}
    for _, m in ipairs(allModels) do
        local checkCode = string.format(
            'local d = Unit.getDescByName("%s") if d then return "1" else return "0" end', m)
        local rok, result = pcall(net.dostring_in, 'server', checkCode)
        if rok and result == "1" then
            available[#available + 1] = m
        end
    end

    if #available > 0 then
        logInfo(string.format("Detected %d/%d ship models available", #available, #allModels))
        tcpSend({ type = "models", models = available })
    end

    return true
end

--- Read available data from the socket, split on newlines, return complete lines.
-- @return table of complete JSON strings (may be empty)
local function tcpReadLines()
    local lines = {}
    if not AIS.connected or not AIS.tcp then
        return lines
    end

    -- Read in a loop until the socket would block
    while true do
        local data, err, partial = AIS.tcp:receive(4096)
        local chunk = data or partial
        if chunk and #chunk > 0 then
            AIS.recvBuffer = AIS.recvBuffer .. chunk
        end
        if err == "timeout" then
            break
        elseif err == "closed" then
            logWarning("TCP connection closed by remote")
            tcpDisconnect()
            break
        elseif err and err ~= "timeout" then
            logError("TCP receive error: " .. tostring(err))
            tcpDisconnect()
            break
        end
        if not data then
            break
        end
    end

    -- Split buffer on newlines
    while true do
        local nlPos = string.find(AIS.recvBuffer, "\n", 1, true)
        if not nlPos then
            break
        end
        local line = string.sub(AIS.recvBuffer, 1, nlPos - 1)
        AIS.recvBuffer = string.sub(AIS.recvBuffer, nlPos + 1)
        if #line > 0 then
            lines[#lines + 1] = line
        end
    end

    return lines
end

-- ---------------------------------------------------------------------------
-- DCS command execution helpers
-- ---------------------------------------------------------------------------

--- Build the Lua code string that spawns a ship group via the server environment.
-- The ship is placed at its AIS position with a projected waypoint 20 km ahead
-- along its heading so it starts moving immediately.
-- Includes a water check: returns "LAND" if the position is on land.
-- @param cmd table with fields: groupName, unitType, lat, lon, heading, speed, name
-- @return string Lua code to execute inside server env
local function buildSpawnCode(cmd)
    AIS.unitCounter = AIS.unitCounter + 1
    local unitId = AIS.unitCounter

    -- Sanitize strings that go into generated code
    local groupName = string.gsub(cmd.groupName or "AIS_UNKNOWN", '["\\\n\r]', '_')
    local unitName  = string.gsub(cmd.name or groupName, '["\\\n\r]', '_')
    local unitType  = string.gsub(cmd.unitType or "dry_cargo_ship_2", '["\\\n\r]', '_')
    local lat       = tonumber(cmd.lat) or 0
    local lon       = tonumber(cmd.lon) or 0
    local heading   = tonumber(cmd.heading) or 0
    local speed     = tonumber(cmd.speed) or 0

    -- Project a waypoint 20 km ahead along the heading so the ship moves on spawn
    local projectDist = 20000 -- metres

    local code = string.format([[
        local pos = coord.LLtoLO(%f, %f, 0)

        -- Water check: reject positions on land
        local surfType = land.getSurfaceType({x = pos.x, y = pos.z})
        if surfType ~= 2 and surfType ~= 3 then
            return "LAND"
        end

        local hdg = %f
        local spd = %f
        local dist = %f

        -- Projected waypoint ahead of the ship
        local wp2x = pos.x + dist * math.cos(hdg)
        local wp2z = pos.z + dist * math.sin(hdg)

        local groupData = {
            ["visible"] = true,
            ["hidden"]  = false,
            ["name"]    = "%s",
            ["task"]    = "Ground Nothing",
            ["uncontrollable"] = true,
            ["route"] = {
                ["points"] = {
                    [1] = {
                        ["x"]      = pos.x,
                        ["y"]      = pos.z,
                        ["alt"]    = 0,
                        ["type"]   = "Turning Point",
                        ["action"] = "Turning Point",
                        ["speed"]  = spd,
                    },
                    [2] = {
                        ["x"]      = wp2x,
                        ["y"]      = wp2z,
                        ["alt"]    = 0,
                        ["type"]   = "Turning Point",
                        ["action"] = "Turning Point",
                        ["speed"]  = spd,
                    },
                },
            },
            ["units"] = {
                [1] = {
                    ["name"]    = "%s",
                    ["type"]    = "%s",
                    ["x"]       = pos.x,
                    ["y"]       = pos.z,
                    ["heading"] = hdg,
                    ["skill"]   = "Average",
                    ["unitId"]  = %d,
                },
            },
        }
        local ok, err = pcall(function()
            coalition.addGroup(country.id.UN_PEACEKEEPERS, Group.Category.SHIP, groupData)
        end)
        if not ok then
            ok, err = pcall(function()
                coalition.addGroup(82, Group.Category.SHIP, groupData)
            end)
        end
        if not ok then
            return "ERROR:" .. tostring(err)
        end
        return "OK"
    ]], lat, lon, heading, speed, projectDist,
       groupName, unitName, unitType, unitId)

    return code
end

--- Build Lua code to spawn a static object (no AI, for anchored ships).
-- Also includes the water check.
-- @param cmd table with fields: groupName, unitType, lat, lon, heading, name
-- @return string Lua code
local function buildStaticSpawnCode(cmd)
    local groupName = string.gsub(cmd.groupName or "AIS_UNKNOWN", '["\\\n\r]', '_')
    local unitType  = string.gsub(cmd.unitType or "dry_cargo_ship_2", '["\\\n\r]', '_')
    local lat       = tonumber(cmd.lat) or 0
    local lon       = tonumber(cmd.lon) or 0
    local heading   = tonumber(cmd.heading) or 0

    return string.format([[
        local pos = coord.LLtoLO(%f, %f, 0)

        -- Water check: reject positions on land
        local surfType = land.getSurfaceType({x = pos.x, y = pos.z})
        if surfType ~= 2 and surfType ~= 3 then
            return "LAND"
        end

        local staticData = {
            ["name"]    = "%s",
            ["type"]    = "%s",
            ["x"]       = pos.x,
            ["y"]       = pos.z,
            ["heading"] = %f,
            ["dead"]    = false,
        }
        local ok, err = pcall(function()
            coalition.addStaticObject(country.id.UN_PEACEKEEPERS, staticData)
        end)
        if not ok then
            ok, err = pcall(function()
                coalition.addStaticObject(82, staticData)
            end)
        end
        if not ok then
            return "ERROR:" .. tostring(err)
        end
        return "OK"
    ]], lat, lon, groupName, unitType, heading)
end

--- Build Lua code to smoothly reroute a ship to a new position by pushing a
--- new waypoint via the group controller. The ship turns and sails there
--- instead of teleporting.
-- @param cmd table with fields: groupName, lat, lon, heading, speed
-- @return string Lua code
local function buildRerouteCode(cmd)
    local groupName = string.gsub(cmd.groupName or "", '["\\\n\r]', '_')
    local lat       = tonumber(cmd.lat) or 0
    local lon       = tonumber(cmd.lon) or 0
    local heading   = tonumber(cmd.heading) or 0
    local speed     = tonumber(cmd.speed) or 0
    local projectDist = 20000

    return string.format([[
        local grp = Group.getByName("%s")
        if not grp then return "NO_GROUP" end
        local unit = grp:getUnit(1)
        if not unit then return "NO_UNIT" end

        local curPos = unit:getPoint()
        local tgtPos = coord.LLtoLO(%f, %f, 0)
        local hdg = %f
        local spd = %f
        local dist = %f

        -- Waypoint beyond the target so the ship keeps sailing through
        local wp3x = tgtPos.x + dist * math.cos(hdg)
        local wp3z = tgtPos.z + dist * math.sin(hdg)

        local route = {
            [1] = {
                ["x"]      = curPos.x,
                ["y"]      = curPos.z,
                ["alt"]    = 0,
                ["speed"]  = spd,
                ["type"]   = "Turning Point",
                ["action"] = "Turning Point",
            },
            [2] = {
                ["x"]      = tgtPos.x,
                ["y"]      = tgtPos.z,
                ["alt"]    = 0,
                ["speed"]  = spd,
                ["type"]   = "Turning Point",
                ["action"] = "Turning Point",
            },
            [3] = {
                ["x"]      = wp3x,
                ["y"]      = wp3z,
                ["alt"]    = 0,
                ["speed"]  = spd,
                ["type"]   = "Turning Point",
                ["action"] = "Turning Point",
            },
        }

        local ok, err = pcall(function()
            local controller = grp:getController()
            controller:setTask({
                id = "Mission",
                params = { route = { points = route } },
            })
        end)
        if not ok then
            return "ERROR:" .. tostring(err)
        end
        return "OK"
    ]], groupName, lat, lon, heading, speed, projectDist)
end

--- Build Lua code to destroy a group by name.
-- @param groupName string
-- @return string Lua code
local function buildDestroyCode(groupName)
    local safe = string.gsub(groupName, '["\\\n\r]', '_')
    return string.format([[
        local grp = Group.getByName("%s")
        if grp then
            grp:destroy()
            return "OK"
        end
        local st = StaticObject.getByName("%s")
        if st then
            st:destroy()
            return "OK"
        end
        return "NOT_FOUND"
    ]], safe, safe)
end

--- Execute Lua code inside the DCS server scripting environment.
-- @param code string  Lua source to run
-- @return string|nil  result, string|nil error
local function serverExec(code)
    local ok, result = pcall(net.dostring_in, 'server', code)
    if not ok then
        logError("net.dostring_in failed: " .. tostring(result))
        return nil, tostring(result)
    end
    return result, nil
end

-- ---------------------------------------------------------------------------
-- Command handlers
-- ---------------------------------------------------------------------------

local function handleSpawn(cmd)
    if not cmd.groupName then
        logError("spawn command missing groupName")
        return
    end

    local isStatic = cmd.static == true
    local label = isStatic and "static" or "group"

    logInfo(string.format("Spawning %s %s (%s) at %.4f, %.4f hdg=%.2f spd=%.1f",
        label, cmd.groupName, cmd.unitType or "?", cmd.lat or 0, cmd.lon or 0,
        cmd.heading or 0, cmd.speed or 0))

    local code
    if isStatic then
        code = buildStaticSpawnCode(cmd)
    else
        code = buildSpawnCode(cmd)
    end

    local result, err = serverExec(code)

    if err then
        logError("Spawn exec error for " .. cmd.groupName .. ": " .. err)
        tcpSend({ type = "error", error = "spawn exec failed: " .. err, groupName = cmd.groupName })
        return
    end

    if result == "LAND" then
        logInfo("Rejected " .. cmd.groupName .. " (on land)")
        return
    end

    if result and string.sub(result, 1, 6) == "ERROR:" then
        local msg = string.sub(result, 7)
        logError("Spawn failed for " .. cmd.groupName .. ": " .. msg)
        tcpSend({ type = "error", error = "spawn failed: " .. msg, groupName = cmd.groupName })
        return
    end

    AIS.trackedGroups[cmd.groupName] = isStatic and "static" or "group"
    logInfo("Spawned " .. label .. " " .. cmd.groupName)
end

local function handleRemove(cmd)
    if not cmd.groupName then
        logError("remove command missing groupName")
        return
    end

    logInfo("Removing group " .. cmd.groupName)

    local code = buildDestroyCode(cmd.groupName)
    local result, err = serverExec(code)

    if err then
        logError("Remove exec error for " .. cmd.groupName .. ": " .. err)
    end

    AIS.trackedGroups[cmd.groupName] = nil
end

local function handleReroute(cmd)
    if not cmd.groupName then
        logError("reroute command missing groupName")
        return
    end

    local tracked = AIS.trackedGroups[cmd.groupName]
    if not tracked then
        logWarning("Reroute: " .. cmd.groupName .. " not tracked, spawning instead")
        handleSpawn(cmd)
        return
    end

    -- Static objects can't be rerouted — they just sit there.
    -- The Go coordinator handles static→group conversion by sending remove+spawn.
    if tracked == "static" then
        return
    end

    logInfo(string.format("Rerouting group %s to %.4f, %.4f hdg=%.2f spd=%.1f",
        cmd.groupName, cmd.lat or 0, cmd.lon or 0, cmd.heading or 0, cmd.speed or 0))

    local code = buildRerouteCode(cmd)
    local result, err = serverExec(code)

    if err then
        logError("Reroute exec error for " .. cmd.groupName .. ": " .. err)
        return
    end

    if result == "NO_GROUP" or result == "NO_UNIT" then
        logWarning("Reroute: group " .. cmd.groupName .. " not found in DCS, re-spawning")
        AIS.trackedGroups[cmd.groupName] = nil
        handleSpawn(cmd)
        return
    end

    if result and string.sub(result, 1, 6) == "ERROR:" then
        logWarning("Reroute failed for " .. cmd.groupName .. ": " .. string.sub(result, 7) .. ", falling back to move")
        handleMove(cmd)
        return
    end

    logInfo("Rerouted " .. cmd.groupName)
end

local function handleMove(cmd)
    if not cmd.groupName then
        logError("move command missing groupName")
        return
    end

    logInfo(string.format("Moving group %s to %.4f, %.4f",
        cmd.groupName, cmd.lat or 0, cmd.lon or 0))

    -- Destroy existing group first
    if AIS.trackedGroups[cmd.groupName] then
        local code = buildDestroyCode(cmd.groupName)
        local _, err = serverExec(code)
        if err then
            logWarning("Move: failed to destroy old group " .. cmd.groupName .. ": " .. err)
        end
        AIS.trackedGroups[cmd.groupName] = nil
    end

    -- Spawn at new position
    handleSpawn(cmd)
end

local function handleClear()
    logInfo("Clearing all tracked AIS groups")
    for groupName, _ in pairs(AIS.trackedGroups) do
        local code = buildDestroyCode(groupName)
        local _, err = serverExec(code)
        if err then
            logWarning("Clear: failed to destroy " .. groupName .. ": " .. err)
        end
    end
    AIS.trackedGroups = {}
    logInfo("All AIS groups cleared")
end

--- Dispatch a parsed command table.
local function dispatchCommand(cmd)
    if not cmd or not cmd.cmd then
        logWarning("Received message with no 'cmd' field")
        return
    end

    if cmd.cmd == "spawn" then
        handleSpawn(cmd)
    elseif cmd.cmd == "remove" then
        handleRemove(cmd)
    elseif cmd.cmd == "reroute" then
        handleReroute(cmd)
    elseif cmd.cmd == "move" then
        handleMove(cmd)
    elseif cmd.cmd == "clear" then
        handleClear()
    else
        logWarning("Unknown command: " .. tostring(cmd.cmd))
    end
end

-- ---------------------------------------------------------------------------
-- Main poll loop (called from onSimulationFrame, throttled)
-- ---------------------------------------------------------------------------

local function poll()
    -- If not connected, attempt reconnect
    if not AIS.connected then
        tcpConnect()
        return
    end

    -- Read and process lines
    local lines = tcpReadLines()
    for _, line in ipairs(lines) do
        local ok, cmd = pcall(JSON.decode, JSON, line)
        if ok and cmd then
            dispatchCommand(cmd)
        else
            logError("Failed to decode JSON: " .. tostring(cmd) .. " | raw: " .. line)
        end
    end
end

-- ---------------------------------------------------------------------------
-- Destroy all tracked groups (used on mission stop / clear)
-- ---------------------------------------------------------------------------

local function destroyAllTracked()
    for groupName, _ in pairs(AIS.trackedGroups) do
        pcall(function()
            local code = buildDestroyCode(groupName)
            serverExec(code)
        end)
    end
    AIS.trackedGroups = {}
end

-- ---------------------------------------------------------------------------
-- DCS Callbacks
-- ---------------------------------------------------------------------------

local callbacks = {}

function callbacks.onSimulationFrame()
    local now = socket.gettime()
    if now - AIS.lastPollTime < AIS.POLL_INTERVAL then
        return
    end
    AIS.lastPollTime = now

    local ok, err = pcall(poll)
    if not ok then
        logError("Poll error: " .. tostring(err))
    end
end

function callbacks.onMissionLoadEnd()
    logInfo("Mission load ended, detecting theatre...")

    -- Detect theatre/map
    local ok, result = pcall(net.dostring_in, 'server', 'return env.mission.theatre')
    if ok and result and #result > 0 then
        AIS.theatre = result
        logInfo("Theatre detected: " .. AIS.theatre)
    else
        logWarning("Could not detect theatre: " .. tostring(result))
        AIS.theatre = "Unknown"
    end

    -- Cleanly disconnect old socket before reconnecting
    tcpDisconnect()

    -- Reset state for new mission
    AIS.trackedGroups = {}
    AIS.unitCounter = 0
    AIS.lastPollTime = 0
    AIS.lastReconnect = 0

    -- Connect (or reconnect) to backend
    tcpConnect()
end

function callbacks.onSimulationStop()
    logInfo("Simulation stopping, cleaning up...")

    -- Destroy all spawned groups
    destroyAllTracked()

    -- Disconnect TCP
    tcpDisconnect()

    -- Reset state
    AIS.theatre = nil
    AIS.unitCounter = 0
    AIS.lastPollTime = 0
    AIS.lastReconnect = 0
end

-- ---------------------------------------------------------------------------
-- Register callbacks with DCS
-- ---------------------------------------------------------------------------

logInfo("AIS Traffic Hook loading...")
DCS.setUserCallbacks(callbacks)
logInfo("AIS Traffic Hook loaded successfully")
