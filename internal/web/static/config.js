// Configuration page: open the form dialog when a form arrives,
// close it when the server says "configSaved".
(function () {
  "use strict";
  const dlg = document.getElementById("form-modal");

  document.body.addEventListener("htmx:afterSwap", (e) => {
    if (e.detail.target.id !== "form-body") return;
    const form = e.detail.target.querySelector("form");
    if (form) document.getElementById("form-title").textContent = form.dataset.title;
    if (!dlg.open) dlg.showModal();
    const bad = e.detail.target.querySelector(".field-error");
    if (bad) bad.parentElement.querySelector("input,select")?.focus();
  });

  // Sent by the server (HX-Trigger header) after a successful save/delete.
  document.body.addEventListener("configSaved", (e) => {
    if (dlg.open) dlg.close();
    const text = e.detail && e.detail.value;
    if (text) {
      const msg = document.getElementById("config-msg");
      msg.innerHTML = "";
      const div = document.createElement("div");
      div.className = "msg ok";
      div.textContent = text;
      msg.appendChild(div);
    }
  });
})();

// PV form: show the analog or the digital fieldset depending on the
// register address typed (1xxxx = digital input).
(function () {
  "use strict";
  function sync(form) {
    const reg = Number(form.querySelector("[name=register_address]").value);
    const digital = reg >= 10001 && reg <= 19999;
    form.querySelectorAll(".analog-only").forEach((el) => { el.hidden = digital; });
    form.querySelectorAll(".digital-only").forEach((el) => { el.hidden = !digital; });
    // Hidden analog inputs must not block submit with "required".
    form.querySelectorAll(".analog-only [name=pv_min], .analog-only [name=pv_max]")
        .forEach((el) => { el.required = !digital; });
  }
  document.body.addEventListener("htmx:afterSwap", (e) => {
    const form = e.detail.target.querySelector("#pv-form");
    if (!form) return;
    sync(form);
    form.querySelector("[name=register_address]").addEventListener("input", () => sync(form));
  });
})();

// PV form: the alarm part. "Enable Alarms" shows the alarm fields, each alarm
// type has its own on/off switch, and a newly switched-on alarm gets suggested
// values (limit as a share of the PV range, 2 % deadband, a default message).
(function () {
  "use strict";
  const roundNice = (n) => Number(n.toPrecision(3));
  const num = (v) => { const n = parseFloat(v); return Number.isNaN(n) ? null : n; };

  function setup(form) {
    const master = form.querySelector("#alarms-on");
    const fields = form.querySelector("#alarm-fields");
    if (!master || !fields) return;
    const input = (name) => form.querySelector("[name=" + name + "]");
    const unit = input("unit"), min = input("pv_min"), max = input("pv_max"), pvName = input("pv_name");
    const s0 = input("state0_text"), s1 = input("state1_text");

    function suggest(card) {
      const type = card.dataset.type;
      const msg = card.querySelector(".alarm-message");
      if (type === "DISCRETE") {
        if (msg && !msg.value.trim() && pvName.value.trim()) msg.value = pvName.value.trim() + " alarm state";
        return;
      }
      const lo = num(min.value), hi = num(max.value);
      const sp = card.querySelector(".alarm-setpoint"), db = card.querySelector(".alarm-deadband");
      if (lo !== null && hi !== null && hi > lo) {
        if (sp && sp.value === "") sp.value = roundNice(lo + Number(card.dataset.pct) * (hi - lo));
        if (db && db.value === "") db.value = roundNice(0.02 * (hi - lo));
      }
      if (msg && !msg.value.trim() && pvName.value.trim()) {
        const words = { HIHI: "very high", HI: "high", LO: "low", LOLO: "very low" };
        msg.value = pvName.value.trim() + " " + (words[type] || type.toLowerCase());
      }
    }

    function showCard(card) {
      const on = card.querySelector(".alarm-on").checked;
      card.querySelector(".alarm-body").hidden = !on;
      card.classList.toggle("off", !on);
      if (on) suggest(card);
    }

    function units() {
      const u = unit && unit.value.trim() ? "(" + unit.value.trim() + ")" : "";
      form.querySelectorAll(".alarm-unit").forEach((el) => { el.textContent = u; });
    }

    function normalState() {
      const sel = form.querySelector(".alarm-normal");
      if (!sel) return;
      const t0 = s0.value.trim() || "0", t1 = s1.value.trim() || "1";
      sel.options[1].textContent = "0 = " + t0;
      sel.options[2].textContent = "1 = " + t1;
      const hint = form.querySelector(".alarm-when");
      if (hint) {
        hint.textContent = sel.value === "" ? "The alarm triggers when the signal leaves its normal state."
          : "Alarm when the signal is " + (sel.value === "0" ? "1 = " + t1 : "0 = " + t0) + ".";
      }
    }

    master.addEventListener("change", () => {
      fields.hidden = !master.checked;
      if (master.checked) form.querySelectorAll(".alarm-card").forEach(showCard);
    });
    form.querySelectorAll(".alarm-card").forEach((card) => {
      card.querySelector(".alarm-on").addEventListener("change", () => showCard(card));
      const b = card.querySelector(".alarm-body");
      b.hidden = !card.querySelector(".alarm-on").checked;
      card.classList.toggle("off", b.hidden);
    });
    [unit, s0, s1].forEach((el) => el && el.addEventListener("input", () => { units(); normalState(); }));
    const sel = form.querySelector(".alarm-normal");
    if (sel) sel.addEventListener("change", normalState);
    units();
    normalState();
  }

  document.body.addEventListener("htmx:afterSwap", (e) => {
    const form = e.detail.target.querySelector("#pv-form");
    if (form) setup(form);
  });
})();
