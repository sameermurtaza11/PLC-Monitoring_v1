// PLC Monitoring — client-side glue.
//
// HTMX does all the data fetching. This file only:
//   0. keeps the dashboard rows and the open PV's live block current from the
//      pushed /events stream (Server-Sent Events) — no polling
//   1. opens the modal when PV detail HTML arrives
//   2. draws the ECharts gauge and moves it when the live value refreshes
//   3. draws the ECharts trend from the JSON inside the history partial
//   4. manages the "compare with" chips
(function () {
  "use strict";

  let gauge = null; // ECharts instance for #pv-gauge
  let trend = null; // ECharts instance for #pv-trend

  const C = {
    text: "#1d2330", muted: "#6b7280", grid: "#e3e6eb",
    track: "#e5e7eb", ok: "#15803d", warn: "#b45309", bad: "#b91c1c",
  };

  const num = (s) => (s === "" || s == null ? null : Number(s));
  const fmt = (v) => (v == null || isNaN(v) ? "—" : Number(v).toFixed(2));

  // ------------------------------------------------------------------
  // HTMX events
  // ------------------------------------------------------------------
  document.body.addEventListener("htmx:afterSwap", (e) => {
    if (e.detail.target.id === "pv-detail") {
      const dlg = document.getElementById("pv-modal");
      if (!dlg.open) dlg.showModal();
    }
  });

  // afterSettle: the new HTML is in the DOM and the dialog has its size.
  document.body.addEventListener("htmx:afterSettle", (e) => {
    const id = e.detail.target.id;
    if (id === "pv-detail") initDetail();
    else if (id === "history") drawTrend();
  });

  // ------------------------------------------------------------------
  // Pushed live updates. One EventSource per page (dashboard rows or alarm
  // list, from the tbody's data-stream). While a detail dialog is open the
  // stream is reopened with the id of that PV / alarm so the server also pushes
  // its live block. After a reconnect the server sends the full current state.
  // ------------------------------------------------------------------
  let stream = null;
  let streamID = 0;
  const liveRows = () => document.getElementById("live-rows");

  function setLiveState(text, cls) {
    const el = document.getElementById("live-state");
    if (!el) return;
    el.textContent = text;
    el.className = "badge " + cls;
  }

  // Pushed event name -> the element it replaces.
  const targets = {
    rows: () => liveRows(),
    summary: () => document.getElementById("live-summary"),
    live: () => document.querySelector("#pv-detail .live-wrap"),
    detail: () => document.getElementById("alarm-dyn"),
    actions: () => document.getElementById("alarm-actions"),
  };
  // "badge" carries only a number for the menu badge (see nav.js).
  const badge = (e) => window.updateNavBadge && window.updateNavBadge(Number(e.data));

  function openStream(id) {
    const tb = liveRows();
    if (!tb) return; // only pages with live rows
    if (stream) stream.close();
    streamID = id;
    const base = tb.dataset.stream || "/events";
    const param = base.includes("alarms") ? "alarm" : "pv";
    stream = new EventSource(base + (id ? (base.includes("?") ? "&" : "?") + param + "=" + id : ""));
    stream.onopen = () => setLiveState("Live", "ok");
    stream.onerror = () => setLiveState("Disconnected — reconnecting…", "bad");
    stream.addEventListener("badge", badge);
    Object.keys(targets).forEach((name) => {
      stream.addEventListener(name, (e) => {
        const el = targets[name]();
        if (!el) return;
        el.innerHTML = e.data;
        if (window.htmx) htmx.process(el); // buttons in the new HTML
        if (name === "live") updateGauge();
      });
    });
  }

  window.addEventListener("DOMContentLoaded", () => openStream(0));

  // Alarm page: result of Acknowledge / Shelve / Unshelve (HX-Trigger alarmMsg).
  document.body.addEventListener("alarmMsg", (e) => {
    const box = document.getElementById("alarm-msg");
    if (!box) return;
    const d = e.detail || {};
    box.innerHTML = "";
    const div = document.createElement("div");
    div.className = "msg " + (d.kind === "error" ? "error" : "ok");
    div.textContent = d.text || "";
    box.appendChild(div);
    if (d.kind !== "error") setTimeout(() => { if (div.parentNode === box) div.remove(); }, 6000);
  });

  // Alarm detail dialog: open it when the HTML arrives, follow that alarm, and
  // stop following it when the dialog closes.
  document.body.addEventListener("htmx:afterSwap", (e) => {
    if (e.detail.target.id !== "alarm-detail") return;
    const dlg = document.getElementById("alarm-modal");
    if (!dlg.open) dlg.showModal();
    const d = e.detail.target.querySelector(".alarm-detail");
    if (d) openStream(Number(d.dataset.occId));
  });
  document.addEventListener("close", (e) => {
    if (e.target.id === "alarm-modal" && streamID) openStream(0);
  }, true);
  window.addEventListener("DOMContentLoaded", () => {
    const id = new URLSearchParams(location.search).get("alarm");
    if (id && /^\d+$/.test(id) && document.getElementById("alarm-detail")) {
      htmx.ajax("GET", "/alarm/" + id, { target: "#alarm-detail", swap: "innerHTML" });
    }
  });

  // Direct link /pv/{id} redirects to /?pv={id}: open that PV's detail on load.
  window.addEventListener("DOMContentLoaded", () => {
    const id = new URLSearchParams(location.search).get("pv");
    if (id && /^\d+$/.test(id) && document.getElementById("pv-detail")) {
      htmx.ajax("GET", "/pv/" + id, { target: "#pv-detail", swap: "innerHTML" });
    }
  });

  document.addEventListener("close", (e) => {
    if (e.target.id === "pv-modal") {
      disposeCharts();
      if (streamID) openStream(0);
    }
  }, true);

  window.addEventListener("resize", () => {
    gauge && gauge.resize();
    trend && trend.resize();
  });

  function disposeCharts() {
    gauge && gauge.dispose();
    trend && trend.dispose();
    gauge = trend = null;
  }

  function initDetail() {
    disposeCharts();
    const d = document.querySelector("#pv-detail .detail");
    if (d && Number(d.dataset.pvId) !== streamID) openStream(Number(d.dataset.pvId));
    initGauge();
    updateLamp();
    initCompare();
  }

  // ------------------------------------------------------------------
  // Gauge  (concept of ECharts "gauge-simple")
  // ------------------------------------------------------------------
  function initGauge() {
    const el = document.getElementById("pv-gauge");
    if (!el) return;
    const min = num(el.dataset.min), max = num(el.dataset.max);
    const lo = num(el.dataset.alarmLow), hi = num(el.dataset.alarmHigh);
    const span = max - min;

    // Alarm limits (if configured) colour the dial: warn below low,
    // normal between, bad above high. Otherwise a plain progress arc.
    const hasAlarms = lo != null || hi != null;
    const bands = [];
    if (lo != null) bands.push([(lo - min) / span, C.warn]);
    if (hi != null) bands.push([(hi - min) / span, C.ok]);
    bands.push([1, hi != null ? C.bad : C.ok]);
    if (lo == null && hi != null) bands[0][1] = C.ok;

    gauge = echarts.init(el);
    gauge.setOption({
      series: [{
        type: "gauge",
        min, max,
        splitNumber: 5,
        radius: "95%",
        progress: { show: !hasAlarms, width: 12, itemStyle: { color: "#2a78d6" } },
        axisLine: { lineStyle: { width: 12, color: hasAlarms ? bands : [[1, C.track]] } },
        pointer: { width: 5, itemStyle: { color: C.text } },
        axisTick: { distance: -12, length: 4, lineStyle: { color: C.muted } },
        splitLine: { distance: -12, length: 12, lineStyle: { color: C.muted, width: 1 } },
        axisLabel: { distance: 16, color: C.muted, fontSize: 10,
                     formatter: (v) => +v.toFixed(2) },
        anchor: { show: true, size: 10, itemStyle: { color: C.text } },
        title: { show: true, offsetCenter: [0, "72%"], color: C.muted, fontSize: 12 },
        detail: { valueAnimation: false, offsetCenter: [0, "45%"], fontSize: 22,
                  fontWeight: 600, color: C.text, formatter: (v) => gaugeLabel(v) },
        data: [{ value: min, name: el.dataset.unit }],
      }],
    });
    updateGauge();
  }

  let gaugeHasValue = false;
  function gaugeLabel(v) { return gaugeHasValue ? fmt(v) : "—"; }

  // Digital input lamp: green/grey dot + state text; amber if the state
  // is the configured alarm state; hollow when the value is not trustworthy.
  function updateLamp() {
    const lamp = document.getElementById("pv-lamp");
    const cur = document.querySelector("#pv-detail .current");
    if (!lamp || !cur) return;
    const v = num(cur.dataset.value);
    const q = cur.dataset.quality;
    const valid = v != null && q !== "COMM_FAIL" && q !== "STALE" && q !== "READ_FAIL";
    const on = valid && v >= 0.5;
    const alarm = valid && lamp.dataset.alarmState !== "" && Number(lamp.dataset.alarmState) === (on ? 1 : 0);
    lamp.className = "lamp " + (!valid ? "unknown" : alarm ? "alarm" : on ? "on" : "off");
    lamp.querySelector(".lamp-text").textContent =
      !valid ? "—" : on ? lamp.dataset.state1 : lamp.dataset.state0;
  }

  // Called after every live refresh: read data-value from the new block.
  function updateGauge() {
    updateLamp();
    const el = document.getElementById("pv-gauge");
    const cur = document.querySelector("#pv-detail .current");
    if (!gauge || !el || !cur) return;
    const v = num(cur.dataset.value);
    const min = num(el.dataset.min), max = num(el.dataset.max);
    gaugeHasValue = v != null && cur.dataset.quality !== "COMM_FAIL" && cur.dataset.quality !== "STALE";
    // Clamp so an out-of-range value pins the needle instead of breaking the dial.
    const shown = v == null ? min : Math.min(max, Math.max(min, v));
    gauge.setOption({ series: [{ data: [{ value: shown, name: el.dataset.unit }] }] });
  }

  // ------------------------------------------------------------------
  // Compare chips: each chip = hidden <input name="compare" value="id">
  // inside the trend form, so HTMX sends them with every request.
  // ------------------------------------------------------------------
  const MAX_COMPARE = 4;

  function initCompare() {
    const sel = document.getElementById("compare-add");
    if (!sel) return;
    sel.addEventListener("change", () => {
      const id = sel.value;
      const chips = document.getElementById("compare-chips");
      if (!id || chips.querySelector(`input[value="${id}"]`)) { sel.value = ""; return; }
      if (chips.children.length >= MAX_COMPARE) {
        alertInline(`Up to ${MAX_COMPARE} PVs can be compared.`); sel.value = ""; return;
      }
      const label = sel.options[sel.selectedIndex].text;
      const chip = document.createElement("span");
      chip.className = "chip";
      chip.innerHTML = `<input type="hidden" name="compare" value="${id}"><span></span>` +
                       `<button type="button" aria-label="Remove">✕</button>`;
      chip.querySelector("span").textContent = label; // textContent: no HTML injection
      chip.querySelector("button").addEventListener("click", () => { chip.remove(); refreshTrend(); });
      chips.appendChild(chip);
      sel.value = "";
      refreshTrend();
    });
  }

  function refreshTrend() {
    const form = document.getElementById("trend-form");
    if (form) htmx.trigger(form, "refresh");
  }

  function alertInline(msg) {
    const chips = document.getElementById("compare-chips");
    const n = document.createElement("span");
    n.className = "error"; n.textContent = msg;
    chips.after(n); setTimeout(() => n.remove(), 3000);
  }

  // ------------------------------------------------------------------
  // Trend  (concept of ECharts "area-simple")
  //
  // One y-axis only. If all PVs share a unit → real values.
  // If units differ (bar vs °C) → each PV is plotted as % of its own
  // pv_min…pv_max range, and the tooltip shows the real value + unit.
  // ------------------------------------------------------------------
  // Break the line where samples are missing (app stopped, PLC offline).
  // The app writes at least one row per minute, so a gap well beyond that
  // (and beyond 5x the typical spacing / 3 buckets) means no data was
  // recorded. A null point in the middle of the gap makes ECharts lift the pen.
  function withGaps(points, bucketSec) {
    if (points.length < 3) return points;
    const dts = [];
    for (let i = 1; i < points.length; i++) dts.push(points[i][0] - points[i - 1][0]);
    const median = dts.slice().sort((a, b) => a - b)[Math.floor(dts.length / 2)];
    const limit = Math.max(180000, 5 * median, 3000 * (bucketSec || 0));
    const out = [points[0]];
    for (let i = 1; i < points.length; i++) {
      const [t0] = points[i - 1], [t1] = points[i];
      if (t1 - t0 > limit) out.push([(t0 + t1) / 2, null]);
      out.push(points[i]);
    }
    return out;
  }

  function drawTrend() {
    const dataEl = document.getElementById("history-data");
    const el = document.getElementById("pv-trend");
    if (!dataEl || !el) return;
    const data = JSON.parse(dataEl.textContent) || [];

    // Colour each chip like its line so the chip row doubles as a key.
    document.querySelectorAll("#compare-chips .chip").forEach((chip) => {
      const s = data.find((d) => String(d.id) === chip.querySelector("input").value);
      chip.style.borderColor = s ? s.color : "";
    });

    const units = [...new Set(data.map((s) => s.unit))];
    const allDigital = data.length > 0 && data.every((s) => s.digital);
    const anyDigital = data.some((s) => s.digital);
    // Real values only when every line shares one scale; a 0/1 digital
    // line next to an analog one would be flat at the bottom, so mixing
    // digital and analog also switches to % of range (0 % = OFF, 100 % = ON).
    const normalized = units.length > 1 || (anyDigital && !allDigital);

    const series = data.map((s, i) => {
      const k = normalized ? 100 / (s.max - s.min) : 1;
      const off = normalized ? s.min : 0;
      return {
        id: String(s.id),
        name: s.label,
        type: "line",
        symbol: "none",
        // Digital: step line (a state holds until the next change) and no
        // down-sampling, which would invent intermediate values.
        step: s.digital ? "end" : false,
        sampling: s.digital ? undefined : "lttb",
        lineStyle: { width: 2, color: s.color },
        itemStyle: { color: s.color },
        // Only the selected PV gets the filled area; compared PVs are lines,
        // so overlapping areas don't hide each other.
        areaStyle: i === 0 ? { opacity: 0.18, color: s.color } : undefined,
        data: withGaps(s.points, s.bucket).map(([t, v]) => v == null ? [t, null, null] : [t, (v - off) * k, v]), // [time, plotted, real]
        unit: s.unit,
      };
    });

    if (!trend) trend = echarts.init(el);
    trend.setOption({
      animation: false,
      grid: { left: 16, right: 24, top: 36, bottom: 72, containLabel: true }, // room for "RUNNING" etc.
      legend: data.length > 1 ? { top: 4, textStyle: { color: C.text } } : { show: false },
      tooltip: {
        trigger: "axis",
        axisPointer: { type: "line" },
        valueFormatter: undefined,
        formatter: (params) => {
          if (!params.length || params[0].value[2] == null) return "";
          const t = new Date(params[0].value[0]).toLocaleString();
          const rows = params.filter((p) => p.value[2] != null).map((p) => {
            const unit = series[p.seriesIndex].unit;
            const d = data[p.seriesIndex];
            if (d.digital && d.bucket === 0) {
              return `${p.marker} ${p.seriesName}: <b>${p.value[2] >= 0.5 ? d.state1 : d.state0}</b>`;
            }
            return `${p.marker} ${p.seriesName}: <b>${fmt(p.value[2])}</b> ${unit}`;
          });
          return [t, ...rows].join("<br>");
        },
      },
      xAxis: { type: "time", boundaryGap: false,
               axisLine: { lineStyle: { color: C.grid } }, axisLabel: { color: C.muted } },
      yAxis: allDigital ? {
        // Digital only: two levels, labelled with the state texts of the
        // selected PV (all lines share 0 = OFF-state, 1 = ON-state).
        type: "value", min: -0.15, max: 1.15,
        axisLabel: { color: C.muted, customValues: [0, 1],
                     formatter: (v) => (v === 1 ? data[0].state1 : data[0].state0) },
        axisTick: { customValues: [0, 1] },
        splitLine: { show: false },
      } : {
        type: "value",
        name: normalized ? "% of range" : units[0] || "",
        nameTextStyle: { color: C.muted },
        scale: !normalized,
        min: normalized ? 0 : undefined,
        max: normalized ? 100 : undefined,
        axisLabel: { color: C.muted },
        splitLine: { lineStyle: { color: C.grid } },
      },
      // Zoom with mouse wheel (inside) or the slider underneath, like area-simple.
      dataZoom: [{ type: "inside" }, { type: "slider", height: 24, bottom: 16 }],
      series,
    }, { replaceMerge: ["series", "yAxis"] }); // replace lines + axis, keep the user's zoom

    const note = document.getElementById("trend-note");
    if (note) note.remove();
    if (normalized) {
      const n = document.createElement("div");
      n.id = "trend-note"; n.className = "muted small";
      n.textContent = "Different units or digital + analog → each PV plotted as % of its range (digital: 0 % = OFF, 100 % = ON); tooltip shows real values.";
      el.after(n);
    }
  }
})();
