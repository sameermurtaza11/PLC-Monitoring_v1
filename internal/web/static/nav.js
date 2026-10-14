// Menu badge: the number of alarms waiting for an acknowledgement.
//
// Pages with live rows (dashboard, alarms) receive it on their own /events
// stream (app.js calls updateNavBadge). Every other page opens a small stream
// of its own, so the badge is current wherever the operator is.
(function () {
  "use strict";

  window.updateNavBadge = function (n) {
    const el = document.getElementById("nav-badge");
    if (!el || !Number.isFinite(n)) return;
    el.textContent = String(n);
    el.hidden = n <= 0;
  };

  window.addEventListener("DOMContentLoaded", () => {
    if (!document.getElementById("nav-badge") || document.getElementById("live-rows")) return;
    const es = new EventSource("/events?view=badge");
    es.addEventListener("badge", (e) => window.updateNavBadge(Number(e.data)));
  });
})();
