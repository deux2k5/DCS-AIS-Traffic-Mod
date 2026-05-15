(function () {
  "use strict";

  var aisDot = document.getElementById("ais-dot");
  var hookDot = document.getElementById("hook-dot");
  var enableToggle = document.getElementById("enable-toggle");
  var toggleText = document.getElementById("toggle-text");
  var theatreValue = document.getElementById("theatre-value");
  var modelsValue = document.getElementById("models-value");
  var shipCount = document.getElementById("ship-count");
  var spawnedCount = document.getElementById("spawned-count");
  var pendingCount = document.getElementById("pending-count");
  var categoryBar = document.getElementById("category-bar");
  var apiKeyInput = document.getElementById("api-key");
  var saveApiKeyBtn = document.getElementById("save-api-key");
  var maxShipsSlider = document.getElementById("max-ships");
  var maxShipsVal = document.getElementById("max-ships-val");
  var updateIntervalSlider = document.getElementById("update-interval");
  var updateIntervalVal = document.getElementById("update-interval-val");
  var shipTbody = document.getElementById("ship-tbody");
  var emptyState = document.getElementById("empty-state");
  var shipTable = document.getElementById("ship-table");

  var ignoreNextToggle = false;

  // ------ Sort state ------
  var sortCol = "state";   // default sort column
  var sortAsc = true;      // ascending by default

  var SORT_COLUMNS = ["name", "category", "dcsModel", "length", "pos", "heading", "sog", "seen", "state"];

  var CATEGORY_COLORS = {
    cargo: "#4da8da",
    tanker: "#ffd166",
    fishing: "#7cff8a",
    passenger: "#c49bff",
    tug: "#ff9a5c",
    pleasure: "#ff5a9b",
    other: "#6f8796"
  };

  // ------ Sort header setup ------
  function setupSortHeaders() {
    var headers = shipTable.querySelectorAll("thead th");
    for (var i = 0; i < headers.length; i++) {
      (function (idx) {
        var th = headers[idx];
        var col = SORT_COLUMNS[idx];
        th.setAttribute("data-sort", col);
        th.style.cursor = "pointer";
        th.style.userSelect = "none";
        th.addEventListener("click", function () {
          if (sortCol === col) {
            sortAsc = !sortAsc;
          } else {
            sortCol = col;
            sortAsc = true;
          }
          updateSortIndicators();
        });
      })(i);
    }
    updateSortIndicators();
  }

  function updateSortIndicators() {
    var headers = shipTable.querySelectorAll("thead th");
    for (var i = 0; i < headers.length; i++) {
      var th = headers[i];
      var col = th.getAttribute("data-sort");
      // Remove old indicator
      var old = th.querySelector(".sort-arrow");
      if (old) old.remove();

      if (col === sortCol) {
        var arrow = document.createElement("span");
        arrow.className = "sort-arrow";
        arrow.textContent = sortAsc ? " ▲" : " ▼";
        th.appendChild(arrow);
      }
    }
  }

  function sortShips(ships) {
    var col = sortCol;
    var asc = sortAsc;

    ships.sort(function (a, b) {
      var va, vb;
      switch (col) {
        case "name":
          va = (a.name || "").toLowerCase();
          vb = (b.name || "").toLowerCase();
          return asc ? va.localeCompare(vb) : vb.localeCompare(va);
        case "category":
          va = (a.category || "other").toLowerCase();
          vb = (b.category || "other").toLowerCase();
          return asc ? va.localeCompare(vb) : vb.localeCompare(va);
        case "dcsModel":
          va = (a.dcsModel || "").toLowerCase();
          vb = (b.dcsModel || "").toLowerCase();
          return asc ? va.localeCompare(vb) : vb.localeCompare(va);
        case "length":
          va = a.length || 0;
          vb = b.length || 0;
          return asc ? va - vb : vb - va;
        case "pos":
          va = a.lat || 0;
          vb = b.lat || 0;
          return asc ? va - vb : vb - va;
        case "heading":
          va = a.heading >= 0 ? a.heading : -1;
          vb = b.heading >= 0 ? b.heading : -1;
          return asc ? va - vb : vb - va;
        case "sog":
          va = a.sog || 0;
          vb = b.sog || 0;
          return asc ? va - vb : vb - va;
        case "seen":
          va = a.lastSeen ? new Date(a.lastSeen).getTime() : 0;
          vb = b.lastSeen ? new Date(b.lastSeen).getTime() : 0;
          return asc ? va - vb : vb - va;
        case "state":
          var stateOrder = { "Spawned": 0, "Pending": 1, "Removing": 2 };
          va = stateOrder[a.state] !== undefined ? stateOrder[a.state] : 3;
          vb = stateOrder[b.state] !== undefined ? stateOrder[b.state] : 3;
          if (va !== vb) return asc ? va - vb : vb - va;
          // secondary: SOG descending
          return b.sog - a.sog;
        default:
          return 0;
      }
    });

    return ships;
  }

  // ------ Status polling ------
  function fetchStatus() {
    fetch("/api/status")
      .then(function (r) { return r.json(); })
      .then(function (data) {
        aisDot.className = data.aisConnected ? "dot connected" : "dot";
        hookDot.className = data.hookConnected ? "dot connected" : "dot";

        ignoreNextToggle = true;
        enableToggle.checked = data.enabled;
        toggleText.textContent = data.enabled ? "Enabled" : "Disabled";
        toggleText.className = data.enabled ? "toggle-text active" : "toggle-text";
        ignoreNextToggle = false;

        theatreValue.textContent = data.theatre || "--";
        modelsValue.textContent = data.modelsLoaded > 0 ? data.modelsLoaded : "--";
        shipCount.textContent = data.shipCount;
        spawnedCount.textContent = data.spawnedCount;
        pendingCount.textContent = data.pendingCount;

        renderCategoryBar(data.categories || {}, data.shipCount || 0);
      })
      .catch(function () {});
  }

  function renderCategoryBar(cats, total) {
    if (total === 0) {
      categoryBar.innerHTML = "";
      return;
    }

    var order = ["cargo", "tanker", "fishing", "passenger", "tug", "pleasure", "other"];
    var html = '<div class="cat-segments">';
    for (var i = 0; i < order.length; i++) {
      var key = order[i];
      var count = cats[key] || 0;
      if (count === 0) continue;
      var pct = (count / total * 100).toFixed(1);
      var color = CATEGORY_COLORS[key] || "#6f8796";
      html += '<div class="cat-seg" style="width:' + pct + '%;background:' + color + '" title="' + key + ': ' + count + '"></div>';
    }
    html += "</div><div class=\"cat-legend\">";
    for (var j = 0; j < order.length; j++) {
      var k = order[j];
      var c = cats[k] || 0;
      if (c === 0) continue;
      html += '<span class="cat-tag"><span class="cat-dot" style="background:' + CATEGORY_COLORS[k] + '"></span>' + k + ' ' + c + '</span>';
    }
    html += "</div>";
    categoryBar.innerHTML = html;
  }

  // ------ Ships polling ------
  function fetchShips() {
    fetch("/api/ships")
      .then(function (r) { return r.json(); })
      .then(function (ships) {
        if (!ships || ships.length === 0) {
          shipTable.style.display = "none";
          emptyState.className = "empty-state visible";
          return;
        }

        shipTable.style.display = "table";
        emptyState.className = "empty-state";

        sortShips(ships);

        var now = Date.now();
        var html = "";
        for (var i = 0; i < ships.length; i++) {
          var s = ships[i];
          var stateClass = "state-" + (s.state || "pending").toLowerCase();
          var lengthStr = s.length > 0 ? s.length + "m" : "--";
          var hdgStr = s.heading >= 0 ? Math.round(s.heading) + "°" : "--";
          var posStr = s.lat.toFixed(3) + ", " + s.lon.toFixed(3);
          var seenStr = formatAge(s.lastSeen, now);
          var catColor = CATEGORY_COLORS[s.category] || CATEGORY_COLORS.other;

          html += "<tr>" +
            '<td class="cell-name">' + escapeHTML(s.name || "UNKNOWN") + '<span class="cell-mmsi">' + s.mmsi + "</span></td>" +
            '<td><span class="cat-pill" style="border-color:' + catColor + ";color:" + catColor + '">' + escapeHTML(s.category) + "</span></td>" +
            "<td>" + escapeHTML(s.dcsModel) + "</td>" +
            "<td>" + lengthStr + "</td>" +
            "<td>" + posStr + "</td>" +
            "<td>" + hdgStr + "</td>" +
            "<td>" + s.sog.toFixed(1) + " kn</td>" +
            "<td>" + seenStr + "</td>" +
            '<td class="' + stateClass + '">' + escapeHTML(s.state) + "</td>" +
            "</tr>";
        }
        shipTbody.innerHTML = html;
      })
      .catch(function () {});
  }

  function formatAge(isoStr, nowMs) {
    if (!isoStr) return "--";
    var then = new Date(isoStr).getTime();
    var diff = Math.floor((nowMs - then) / 1000);
    if (diff < 0 || isNaN(diff)) return "--";
    if (diff < 60) return diff + "s";
    if (diff < 3600) return Math.floor(diff / 60) + "m";
    return Math.floor(diff / 3600) + "h";
  }

  // ------ Config loading ------
  function loadConfig() {
    fetch("/api/config")
      .then(function (r) { return r.json(); })
      .then(function (cfg) {
        maxShipsSlider.value = cfg.ais.maxShips;
        maxShipsVal.textContent = cfg.ais.maxShips;
        updateIntervalSlider.value = cfg.ais.updateSeconds;
        updateIntervalVal.textContent = cfg.ais.updateSeconds;

        var filters = cfg.filters;
        document.querySelectorAll("[data-filter]").forEach(function (el) {
          var key = el.getAttribute("data-filter");
          if (filters.hasOwnProperty(key)) {
            el.checked = filters[key];
          }
        });
      })
      .catch(function () {});
  }

  // ------ Event handlers ------
  enableToggle.addEventListener("change", function () {
    if (ignoreNextToggle) return;
    var enabled = enableToggle.checked;
    toggleText.textContent = enabled ? "Enabled" : "Disabled";
    toggleText.className = enabled ? "toggle-text active" : "toggle-text";

    fetch("/api/toggle", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ enabled: enabled })
    });
  });

  saveApiKeyBtn.addEventListener("click", function () {
    var key = apiKeyInput.value.trim();
    if (!key) return;

    fetch("/api/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ais: { apiKey: key } })
    }).then(function () {
      apiKeyInput.value = "";
      apiKeyInput.placeholder = "Key saved";
      setTimeout(function () {
        apiKeyInput.placeholder = "aisstream.io API key";
      }, 2000);
    });
  });

  maxShipsSlider.addEventListener("input", function () {
    maxShipsVal.textContent = maxShipsSlider.value;
  });
  maxShipsSlider.addEventListener("change", function () {
    fetch("/api/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ais: { maxShips: parseInt(maxShipsSlider.value, 10) } })
    });
  });

  updateIntervalSlider.addEventListener("input", function () {
    updateIntervalVal.textContent = updateIntervalSlider.value;
  });
  updateIntervalSlider.addEventListener("change", function () {
    fetch("/api/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ais: { updateSeconds: parseInt(updateIntervalSlider.value, 10) } })
    });
  });

  document.querySelectorAll("[data-filter]").forEach(function (el) {
    el.addEventListener("change", function () {
      var filters = {};
      document.querySelectorAll("[data-filter]").forEach(function (cb) {
        filters[cb.getAttribute("data-filter")] = cb.checked;
      });
      fetch("/api/filters", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(filters)
      });
    });
  });

  function escapeHTML(str) {
    if (!str) return "";
    return str.replace(/&/g, "&amp;")
              .replace(/</g, "&lt;")
              .replace(/>/g, "&gt;")
              .replace(/"/g, "&quot;");
  }

  // ------ Update ------
  var updateStatusEl = document.getElementById("update-status");
  var checkUpdateBtn = document.getElementById("check-update");
  var applyUpdateBtn = document.getElementById("apply-update");
  var latestVersion = null;

  checkUpdateBtn.addEventListener("click", function () {
    updateStatusEl.textContent = "Checking...";
    applyUpdateBtn.disabled = true;
    fetch("/api/update/check")
      .then(function (r) { return r.json(); })
      .then(function (data) {
        if (data.error) {
          updateStatusEl.textContent = "Error: " + data.error;
          return;
        }
        latestVersion = data.version;
        updateStatusEl.textContent = data.version + " available";
        applyUpdateBtn.disabled = false;
      })
      .catch(function () {
        updateStatusEl.textContent = "Check failed";
      });
  });

  applyUpdateBtn.addEventListener("click", function () {
    if (!latestVersion) return;
    applyUpdateBtn.disabled = true;
    updateStatusEl.textContent = "Downloading " + latestVersion + "...";
    fetch("/api/update/apply", { method: "POST" })
      .then(function (r) { return r.json(); })
      .then(function (data) {
        updateStatusEl.textContent = "Restarting...";
        setTimeout(function () {
          updateStatusEl.textContent = "Reconnecting...";
          var attempts = 0;
          var reconnect = setInterval(function () {
            attempts++;
            fetch("/api/status")
              .then(function (r) {
                if (r.ok) {
                  clearInterval(reconnect);
                  updateStatusEl.textContent = "Updated!";
                  latestVersion = null;
                }
              })
              .catch(function () {
                if (attempts > 30) {
                  clearInterval(reconnect);
                  updateStatusEl.textContent = "Restart may have failed";
                }
              });
          }, 2000);
        }, 5000);
      })
      .catch(function () {
        updateStatusEl.textContent = "Update failed";
        applyUpdateBtn.disabled = false;
      });
  });

  // ------ Theme toggle ------
  var themeToggleBtn = document.getElementById("theme-toggle");
  var themeIcon = document.getElementById("theme-icon");
  var html = document.documentElement;

  function setTheme(theme) {
    html.setAttribute("data-theme", theme);
    themeIcon.innerHTML = theme === "dark" ? "&#9790;" : "&#9728;";
    try { localStorage.setItem("ais-theme", theme); } catch (e) {}
  }

  function loadTheme() {
    try {
      var saved = localStorage.getItem("ais-theme");
      if (saved === "light" || saved === "dark") return saved;
    } catch (e) {}
    return "dark";
  }

  setTheme(loadTheme());

  themeToggleBtn.addEventListener("click", function () {
    var current = html.getAttribute("data-theme");
    setTheme(current === "dark" ? "light" : "dark");
  });

  // ------ Init ------
  setupSortHeaders();
  loadConfig();
  fetchStatus();
  fetchShips();
  setInterval(fetchStatus, 2000);
  setInterval(fetchShips, 2000);
})();
