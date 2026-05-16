--[[
    AIS Traffic Hook for DCS World

    Runs in GameGUI context (Scripts/Hooks/) and communicates with an external
    Go application via TCP to spawn, move, and remove ships based on real-world
    AIS vessel data.

    The hook listens on a TCP port and the Go exe connects to it when running.
    When the exe is not running, the hook sits idle with near-zero overhead.

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
AIS.MAX_CMDS_PER_POLL   = 2     -- max expensive commands (spawn/remove/move) per poll
AIS.MAX_REROUTES_PER_POLL = 4   -- max reroute commands per poll (cheaper than spawn)
AIS.DEBUG               = false -- set true to enable per-command log lines

-- ---------------------------------------------------------------------------
-- State
-- ---------------------------------------------------------------------------
AIS.listener         = nil      -- TCP server socket
AIS.tcp              = nil      -- accepted client socket
AIS.connected        = false
AIS.recvBuffer       = ""       -- incomplete data from TCP reads
AIS.bufferOffset     = 1        -- read position in recvBuffer (avoids O(n^2) sub)
AIS.lastPollTime     = 0
AIS.trackedGroups    = {}       -- [groupName] = "group" | "static"
AIS.theatre          = nil      -- current map name
AIS.unitCounter      = 0        -- monotonic counter for unique unit IDs
AIS.cmdQueue         = {}       -- pending commands not yet executed
AIS.helpersInstalled = false    -- whether server-side helpers are loaded

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

local function logDebug(msg)
    if AIS.DEBUG then
        log.write('AISTraffic', log.INFO, msg)
    end
end

-- ---------------------------------------------------------------------------
-- TCP helpers
-- ---------------------------------------------------------------------------

--- Start listening on the configured port.
-- @return boolean success
local function tcpListen()
    if AIS.listener then return true end

    local ln, err = socket.bind(AIS.TCP_HOST, AIS.TCP_PORT)
    if not ln then
        logError("Failed to bind TCP port " .. tostring(AIS.TCP_PORT) .. ": " .. tostring(err))
        return false
    end

    ln:settimeout(0) -- non-blocking accept
    AIS.listener = ln
    logInfo("Listening on " .. AIS.TCP_HOST .. ":" .. tostring(AIS.TCP_PORT))
    return true
end

--- Close the listener.
local function tcpStopListening()
    if AIS.listener then
        pcall(function() AIS.listener:close() end)
        AIS.listener = nil
    end
end

--- Disconnect the current client and clean up.
local function tcpDisconnect()
    if AIS.tcp then
        pcall(function() AIS.tcp:close() end)
        AIS.tcp = nil
    end
    AIS.connected = false
    AIS.recvBuffer = ""
    AIS.bufferOffset = 1
    logInfo("Client disconnected from AIS hook")
end

--- Check for a new incoming connection (non-blocking).
-- @return boolean true if a new client was accepted
local function tcpAccept()
    if not AIS.listener then return false end
    if AIS.connected then return false end -- already have a client

    local client, err = AIS.listener:accept()
    if not client then
        return false -- no pending connection (timeout with non-blocking)
    end

    -- Got a new client connection from the exe.
    client:settimeout(0) -- non-blocking for all subsequent I/O
    client:setoption("tcp-nodelay", true)

    AIS.tcp = client
    AIS.connected = true
    AIS.recvBuffer = ""
    AIS.bufferOffset = 1
    AIS.helpersInstalled = false

    logInfo("Exe connected to AIS hook")

    -- Send theatre info to the exe.
    if not AIS.theatre then
        local tok, tresult = pcall(net.dostring_in, 'server', 'return env.mission.theatre')
        if tok and tresult and #tresult > 0 then
            AIS.theatre = tresult
            logInfo("Theatre detected on accept: " .. AIS.theatre)
        end
    end

    if AIS.theatre then
        logInfo("Sending theatre: " .. tostring(AIS.theatre))
        tcpSend({ type = "theatre", theatre = AIS.theatre })
    end

    -- Detect available ship models so the backend only uses installed ones.
    local checkCode = [[
        local models = {
            "container_ship", "fishing_vessel", "trawler_ship", "diesel_trawler",
            "lng_tanker", "ievoli_ivory",
            "jr_more_tug", "jr_more_tug_helipad",
            "yacht_ship", "yacht_helipad",
            "akademik_cherskiy", "akademik_cherskiy_pipe_laying",
            "kimedaka_skyicher", "old_vessel",
            "HandyWind", "Seawise_Giant", "La_Combattante_II", "BDK-775",
            "HarborTug", "Ship_Tilde_Supply", "CastleClass_01", "leander-gun-ariadne",
            "ALBATROS", "CHAP_Project22160",
        }
        local avail = {}
        for _, m in ipairs(models) do
            local d = Unit.getDescByName(m)
            if d then avail[#avail + 1] = m end
        end
        return table.concat(avail, ",")
    ]]

    local rok, result = pcall(net.dostring_in, 'server', checkCode)
    if rok and result and #result > 0 then
        local available = {}
        for m in string.gmatch(result, "([^,]+)") do
            available[#available + 1] = m
        end
        logInfo(string.format("Detected %d ship models available", #available))
        tcpSend({ type = "models", models = available })
    end

    return true
end

--- Send a JSON message (appends newline).
-- @param tbl table to JSON-encode
function tcpSend(tbl)
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

--- Read available data from the socket, split on newlines, return complete lines.
-- Uses offset tracking to avoid O(n^2) string.sub on large buffers.
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
            logWarning("TCP connection closed by exe")
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

    -- Split buffer on newlines using offset to avoid repeated string.sub.
    -- Cap at 20 lines per poll to prevent JSON decode spikes.
    local MAX_LINES = 20
    while #lines < MAX_LINES do
        local nlPos = string.find(AIS.recvBuffer, "\n", AIS.bufferOffset, true)
        if not nlPos then
            break
        end
        local line = string.sub(AIS.recvBuffer, AIS.bufferOffset, nlPos - 1)
        AIS.bufferOffset = nlPos + 1
        if #line > 0 then
            lines[#lines + 1] = line
        end
    end

    -- Compact buffer when we've consumed most of it
    if AIS.bufferOffset > 4096 then
        AIS.recvBuffer = string.sub(AIS.recvBuffer, AIS.bufferOffset)
        AIS.bufferOffset = 1
    end

    return lines
end

-- ---------------------------------------------------------------------------
-- Server-side helper installation
-- ---------------------------------------------------------------------------

--- Install persistent helper functions in the DCS server environment once per
--- mission. Calling compact helpers is much cheaper than compiling large code
--- strings on every spawn/reroute.
local function installHelpers()
    if AIS.helpersInstalled then return true end

    local code = [[
        if AIS_HELPERS then return "OK" end
        AIS_HELPERS = true

        function AIS_Spawn(lat, lon, hdg, spd, dist, groupName, unitName, unitType, unitId)
            local pos = coord.LLtoLO(lat, lon, 0)
            local surfType = land.getSurfaceType({x = pos.x, y = pos.z})
            if surfType ~= 2 and surfType ~= 3 then return "LAND" end

            local wp2x = pos.x + dist * math.cos(hdg)
            local wp2z = pos.z + dist * math.sin(hdg)

            local groupData = {
                visible = true, hidden = false,
                name = groupName, task = "Ground Nothing", uncontrollable = true,
                route = { points = {
                    [1] = { x = pos.x, y = pos.z, alt = 0, type = "Turning Point", action = "Turning Point", speed = spd },
                    [2] = { x = wp2x, y = wp2z, alt = 0, type = "Turning Point", action = "Turning Point", speed = spd },
                }},
                units = { [1] = {
                    name = unitName, type = unitType,
                    x = pos.x, y = pos.z, heading = hdg, skill = "Average", unitId = unitId,
                }},
            }
            local ok, err = pcall(function()
                coalition.addGroup(country.id.UN_PEACEKEEPERS, Group.Category.SHIP, groupData)
            end)
            if not ok then
                ok, err = pcall(function()
                    coalition.addGroup(82, Group.Category.SHIP, groupData)
                end)
            end
            if not ok then return "ERROR:" .. tostring(err) end
            return "OK"
        end

        function AIS_SpawnStatic(lat, lon, hdg, groupName, unitType)
            local pos = coord.LLtoLO(lat, lon, 0)
            local surfType = land.getSurfaceType({x = pos.x, y = pos.z})
            if surfType ~= 2 and surfType ~= 3 then return "LAND" end

            local staticData = {
                name = groupName, type = unitType,
                x = pos.x, y = pos.z, heading = hdg, dead = false,
            }
            local ok, err = pcall(function()
                coalition.addStaticObject(country.id.UN_PEACEKEEPERS, staticData)
            end)
            if not ok then
                ok, err = pcall(function()
                    coalition.addStaticObject(82, staticData)
                end)
            end
            if not ok then return "ERROR:" .. tostring(err) end
            return "OK"
        end

        function AIS_Reroute(groupName, lat, lon, hdg, spd, dist)
            local grp = Group.getByName(groupName)
            if not grp then return "NO_GROUP" end
            local unit = grp:getUnit(1)
            if not unit then return "NO_UNIT" end

            local curPos = unit:getPoint()
            local tgtPos = coord.LLtoLO(lat, lon, 0)
            local wp3x = tgtPos.x + dist * math.cos(hdg)
            local wp3z = tgtPos.z + dist * math.sin(hdg)

            local route = {
                [1] = { x = curPos.x, y = curPos.z, alt = 0, speed = spd, type = "Turning Point", action = "Turning Point" },
                [2] = { x = tgtPos.x, y = tgtPos.z, alt = 0, speed = spd, type = "Turning Point", action = "Turning Point" },
                [3] = { x = wp3x, y = wp3z, alt = 0, speed = spd, type = "Turning Point", action = "Turning Point" },
            }

            local ok, err = pcall(function()
                local controller = grp:getController()
                controller:setTask({ id = "Mission", params = { route = { points = route } } })
            end)
            if not ok then return "ERROR:" .. tostring(err) end
            return "OK"
        end

        function AIS_Destroy(groupName)
            local grp = Group.getByName(groupName)
            if grp then grp:destroy() return "OK" end
            local st = StaticObject.getByName(groupName)
            if st then st:destroy() return "OK" end
            return "NOT_FOUND"
        end

        function AIS_DestroyBatch(names)
            local count = 0
            for _, name in ipairs(names) do
                local grp = Group.getByName(name)
                if grp then grp:destroy() count = count + 1
                else
                    local st = StaticObject.getByName(name)
                    if st then st:destroy() count = count + 1 end
                end
            end
            return tostring(count)
        end
    ]]

    local ok, result = pcall(net.dostring_in, 'server', code)
    if not ok then
        logError("Failed to install helpers: " .. tostring(result))
        return false
    end

    AIS.helpersInstalled = true
    logInfo("Server-side helpers installed")
    return true
end

-- ---------------------------------------------------------------------------
-- DCS command execution helpers (using persistent server functions)
-- ---------------------------------------------------------------------------

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

    if not installHelpers() then
        logWarning("Helpers not installed, deferring spawn")
        return
    end

    local isStatic = cmd.static == true

    logDebug(string.format("Spawning %s %s (%s) at %.4f, %.4f",
        isStatic and "static" or "group", cmd.groupName, cmd.unitType or "?",
        cmd.lat or 0, cmd.lon or 0))

    local result, err

    if isStatic then
        local code = string.format(
            'return AIS_SpawnStatic(%.6f, %.6f, %.4f, "%s", "%s")',
            cmd.lat or 0, cmd.lon or 0, cmd.heading or 0,
            string.gsub(cmd.groupName, '["\\\n\r]', '_'),
            string.gsub(cmd.unitType or "dry_cargo_ship_2", '["\\\n\r]', '_'))
        result, err = serverExec(code)
    else
        AIS.unitCounter = AIS.unitCounter + 1
        local code = string.format(
            'return AIS_Spawn(%.6f, %.6f, %.4f, %.4f, 20000, "%s", "%s", "%s", %d)',
            cmd.lat or 0, cmd.lon or 0, cmd.heading or 0, cmd.speed or 0,
            string.gsub(cmd.groupName or "AIS_UNKNOWN", '["\\\n\r]', '_'),
            string.gsub(cmd.name or cmd.groupName or "AIS_UNKNOWN", '["\\\n\r]', '_'),
            string.gsub(cmd.unitType or "dry_cargo_ship_2", '["\\\n\r]', '_'),
            AIS.unitCounter)
        result, err = serverExec(code)
    end

    if err then
        logError("Spawn exec error for " .. cmd.groupName .. ": " .. err)
        tcpSend({ type = "error", error = "spawn exec failed: " .. err, groupName = cmd.groupName })
        return
    end

    if result == "LAND" then
        logDebug("Rejected " .. cmd.groupName .. " (on land)")
        tcpSend({ type = "reject", reason = "land", groupName = cmd.groupName })
        return
    end

    if result and string.sub(result, 1, 6) == "ERROR:" then
        local msg = string.sub(result, 7)
        logError("Spawn failed for " .. cmd.groupName .. ": " .. msg)
        tcpSend({ type = "error", error = "spawn failed: " .. msg, groupName = cmd.groupName })
        return
    end

    AIS.trackedGroups[cmd.groupName] = isStatic and "static" or "group"
    logDebug("Spawned " .. (isStatic and "static" or "group") .. " " .. cmd.groupName)
end

local function handleRemove(cmd)
    if not cmd.groupName then
        logError("remove command missing groupName")
        return
    end

    if not installHelpers() then return end

    logDebug("Removing " .. cmd.groupName)

    local code = string.format('return AIS_Destroy("%s")',
        string.gsub(cmd.groupName, '["\\\n\r]', '_'))
    local result, err = serverExec(code)

    if err then
        logError("Remove exec error for " .. cmd.groupName .. ": " .. err)
    end

    AIS.trackedGroups[cmd.groupName] = nil
end

local function handleMove(cmd)
    if not cmd.groupName then
        logError("move command missing groupName")
        return
    end

    logDebug(string.format("Moving %s to %.4f, %.4f",
        cmd.groupName, cmd.lat or 0, cmd.lon or 0))

    -- Destroy existing group first
    if AIS.trackedGroups[cmd.groupName] then
        if installHelpers() then
            local code = string.format('return AIS_Destroy("%s")',
                string.gsub(cmd.groupName, '["\\\n\r]', '_'))
            serverExec(code)
        end
        AIS.trackedGroups[cmd.groupName] = nil
    end

    -- Spawn at new position
    handleSpawn(cmd)
end

local function handleReroute(cmd)
    if not cmd.groupName then
        logError("reroute command missing groupName")
        return
    end

    if not installHelpers() then return end

    local tracked = AIS.trackedGroups[cmd.groupName]
    if not tracked then
        logWarning("Reroute: " .. cmd.groupName .. " not tracked, spawning instead")
        handleSpawn(cmd)
        return
    end

    -- Static objects can't be rerouted.
    if tracked == "static" then
        return
    end

    logDebug(string.format("Rerouting %s to %.4f, %.4f",
        cmd.groupName, cmd.lat or 0, cmd.lon or 0))

    local code = string.format(
        'return AIS_Reroute("%s", %.6f, %.6f, %.4f, %.4f, 20000)',
        string.gsub(cmd.groupName, '["\\\n\r]', '_'),
        cmd.lat or 0, cmd.lon or 0, cmd.heading or 0, cmd.speed or 0)
    local result, err = serverExec(code)

    if err then
        logError("Reroute exec error for " .. cmd.groupName .. ": " .. err)
        return
    end

    if result == "NO_GROUP" or result == "NO_UNIT" then
        logWarning("Reroute: " .. cmd.groupName .. " not found in DCS, re-spawning")
        AIS.trackedGroups[cmd.groupName] = nil
        handleSpawn(cmd)
        return
    end

    if result and string.sub(result, 1, 6) == "ERROR:" then
        logWarning("Reroute failed for " .. cmd.groupName .. ": " .. string.sub(result, 7) .. ", falling back to move")
        handleMove(cmd)
        return
    end

    logDebug("Rerouted " .. cmd.groupName)
end

local function handleClear()
    logInfo("Clearing all tracked AIS groups")

    local names = {}
    for groupName, _ in pairs(AIS.trackedGroups) do
        names[#names + 1] = groupName
    end
    AIS.trackedGroups = {}
    AIS.cmdQueue = {} -- drop pending commands (they reference cleared ships)

    if #names == 0 or not installHelpers() then
        logInfo("Clear done (0 groups)")
        return
    end

    -- Destroy in chunks to avoid frame stalls. First chunk runs now,
    -- remaining chunks are queued and processed one-per-poll via budget.
    local CHUNK = 15
    for i = 1, #names, CHUNK do
        local parts = {}
        for j = i, math.min(i + CHUNK - 1, #names) do
            parts[#parts + 1] = '"' .. string.gsub(names[j], '["\\\n\r]', '_') .. '"'
        end
        local code = 'return AIS_DestroyBatch({' .. table.concat(parts, ',') .. '})'
        if i == 1 then
            local result, err = serverExec(code)
            if err then logWarning("Clear chunk error: " .. err) end
        else
            AIS.cmdQueue[#AIS.cmdQueue + 1] = { cmd = "_batch_destroy", _code = code }
        end
    end

    logInfo(string.format("Clearing %d groups in %d chunks", #names, math.ceil(#names / CHUNK)))
end

--- Dispatch a parsed command table. Returns true if it was an expensive
--- command (spawn/remove/move), false if cheap (reroute) or instant (clear).
local function dispatchCommand(cmd)
    if not cmd or not cmd.cmd then
        logWarning("Received message with no 'cmd' field")
        return false
    end

    if cmd.cmd == "spawn" then
        handleSpawn(cmd)
        return true
    elseif cmd.cmd == "remove" then
        handleRemove(cmd)
        return true
    elseif cmd.cmd == "reroute" then
        handleReroute(cmd)
        return false -- reroute is cheaper, tracked separately
    elseif cmd.cmd == "move" then
        handleMove(cmd)
        return true
    elseif cmd.cmd == "clear" then
        handleClear()
        return false
    elseif cmd.cmd == "_batch_destroy" then
        -- Internal: chunked clear continuation.
        if cmd._code then serverExec(cmd._code) end
        return true
    else
        logWarning("Unknown command: " .. tostring(cmd.cmd))
        return false
    end
end

-- ---------------------------------------------------------------------------
-- Main poll loop (called from onSimulationFrame, throttled)
-- Rate-limits expensive DCS operations to avoid frame stalls.
-- ---------------------------------------------------------------------------

local function poll()
    -- If listener failed to bind (port busy at load time), retry it.
    if not AIS.listener then
        tcpListen()
        return
    end

    -- If not connected, check for incoming connection (non-blocking).
    if not AIS.connected then
        tcpAccept()
        return
    end

    -- Read new lines from TCP and add to command queue
    local lines = tcpReadLines()
    for _, line in ipairs(lines) do
        local ok, cmd = pcall(JSON.decode, JSON, line)
        if ok and cmd then
            AIS.cmdQueue[#AIS.cmdQueue + 1] = cmd
        else
            logError("Failed to decode JSON: " .. tostring(cmd))
        end
    end

    -- Process queued commands with per-frame budget
    local expensiveCount = 0
    local rerouteCount = 0
    local remaining = {}

    for _, cmd in ipairs(AIS.cmdQueue) do
        -- Clear commands execute immediately regardless of budget
        if cmd.cmd == "clear" then
            dispatchCommand(cmd)
            -- Wipe any over-budget commands already deferred — they belong
            -- to the pre-clear state and must not be restored afterwards.
            remaining = {}
        elseif cmd.cmd == "reroute" then
            if rerouteCount < AIS.MAX_REROUTES_PER_POLL then
                dispatchCommand(cmd)
                rerouteCount = rerouteCount + 1
            else
                remaining[#remaining + 1] = cmd
            end
        else
            -- spawn, remove, move are expensive
            if expensiveCount < AIS.MAX_CMDS_PER_POLL then
                dispatchCommand(cmd)
                expensiveCount = expensiveCount + 1
            else
                remaining[#remaining + 1] = cmd
            end
        end
    end

    AIS.cmdQueue = remaining

    -- Log queue depth periodically if backing up
    if #AIS.cmdQueue > 10 then
        logWarning(string.format("Command queue backing up: %d pending", #AIS.cmdQueue))
    end
end

-- ---------------------------------------------------------------------------
-- Destroy all tracked groups (used on mission stop / clear)
-- ---------------------------------------------------------------------------

local function destroyAllTracked()
    if installHelpers() then
        local names = {}
        for groupName, _ in pairs(AIS.trackedGroups) do
            names[#names + 1] = groupName
        end
        if #names > 0 then
            local parts = {}
            for i, name in ipairs(names) do
                parts[i] = '"' .. string.gsub(name, '["\\\n\r]', '_') .. '"'
            end
            local code = 'return AIS_DestroyBatch({' .. table.concat(parts, ',') .. '})'
            pcall(serverExec, code)
        end
    else
        for groupName, _ in pairs(AIS.trackedGroups) do
            pcall(function()
                local safe = string.gsub(groupName, '["\\\n\r]', '_')
                serverExec(string.format([[
                    local grp = Group.getByName("%s")
                    if grp then grp:destroy() end
                    local st = StaticObject.getByName("%s")
                    if st then st:destroy() end
                ]], safe, safe))
            end)
        end
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

    -- Disconnect current client (exe will reconnect).
    tcpDisconnect()

    -- Stop old listener and start fresh (handles port change after redeploy).
    tcpStopListening()

    -- Reset state for new mission
    AIS.trackedGroups = {}
    AIS.unitCounter = 0
    AIS.lastPollTime = 0
    AIS.cmdQueue = {}
    AIS.helpersInstalled = false

    -- Start listening for exe connections.
    tcpListen()
end

function callbacks.onSimulationStop()
    logInfo("Simulation stopping, cleaning up...")

    -- Destroy all spawned groups
    destroyAllTracked()

    -- Disconnect client and stop listener
    tcpDisconnect()
    tcpStopListening()

    -- Reset state
    AIS.theatre = nil
    AIS.unitCounter = 0
    AIS.lastPollTime = 0
    AIS.cmdQueue = {}
    AIS.helpersInstalled = false
end

-- ---------------------------------------------------------------------------
-- Register callbacks with DCS and start listener
-- ---------------------------------------------------------------------------

logInfo("AIS Traffic Hook loading...")
DCS.setUserCallbacks(callbacks)

-- Start listening immediately so the exe can connect even before a mission loads.
tcpListen()

logInfo("AIS Traffic Hook loaded successfully")
