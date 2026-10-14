// Shared by every page that has the menu:
//  - the menu badge: the number of alarms waiting for an acknowledgement
//  - the global alarm ticker fixed to the bottom of the page
//
// Pages with live rows (dashboard, alarms) receive both on their own /events
// stream (app.js calls updateNavBadge / updateTicker). Every other page opens a
// small stream of its own, so badge and ticker are current wherever the
// operator is.
(function () {
  "use strict";

  // ------------------------------------------------------------------
  // Menu badge
  // ------------------------------------------------------------------
  window.updateNavBadge = function (n) {
    const el = document.getElementById("nav-badge");
    if (!el || !Number.isFinite(n)) return;
    el.textContent = String(n);
    el.hidden = n <= 0;
  };

  // ------------------------------------------------------------------
  // Alarm ticker: slow, constant-speed scroll; pauses under the pointer.
  // The text is the server-rendered list of active alarms (oldest event
  // first). It is duplicated once so the loop has no gap; if it fits the
  // width it does not move at all. When the list changes, the scroll
  // continues from where it was instead of jumping back to the start.
  // ------------------------------------------------------------------
  const SPEED = 45; // pixels per second: slow enough to read
  const ticker = document.getElementById("alarm-ticker");
  const viewport = ticker && ticker.querySelector(".ticker-viewport");
  const track = ticker && ticker.querySelector(".ticker-track");
  const reduceMotion = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  let anim = null;
  let current = null; // the items HTML shown now
  let hovering = false;
  let focused = false;

  function applyPause() {
    if (!anim) return;
    if (hovering || focused) anim.pause();
    else anim.play();
  }

  function render(html, force) {
    if (!track || (!force && html === current)) return;
    current = html;
    const progress = anim && anim.effect ? (anim.currentTime || 0) / anim.effect.getTiming().duration : 0;
    if (anim) { anim.cancel(); anim = null; }

    track.innerHTML = '<div class="ticker-set">' + html + "</div>";
    const set = track.firstElementChild;
    const hasAlarms = !set.querySelector(".tick-none");
    ticker.classList.toggle("calm", !hasAlarms);

    const overflows = set.getBoundingClientRect().width > viewport.clientWidth;
    ticker.classList.toggle("static", !overflows);
    if (!overflows || reduceMotion) return; // reduced motion: the viewport scrolls by hand

    track.insertAdjacentHTML("beforeend", '<div class="ticker-set" aria-hidden="true">' + html + "</div>");
    const w = set.getBoundingClientRect().width; // one copy, including its trailing gap
    const duration = (w / SPEED) * 1000;
    anim = track.animate(
      [{ transform: "translateX(0)" }, { transform: "translateX(-" + w + "px)" }],
      { duration: duration, iterations: Infinity, easing: "linear" }
    );
    anim.currentTime = (progress % 1) * duration;
    applyPause();
  }

  window.updateTicker = function (html) { render(html, false); };
  window.tickerState = function () { // for tests and diagnostics
    return { playState: anim ? anim.playState : "none", scrolling: !!anim, hovering: hovering, html: current };
  };

  if (ticker) {
    // Initial content is already in the page (server-rendered): measure it and start scrolling.
    window.addEventListener("DOMContentLoaded", () => {
      const first = track.querySelector(".ticker-set");
      render(first ? first.innerHTML : "", true);
    });
    ticker.addEventListener("pointerenter", () => { hovering = true; applyPause(); });
    ticker.addEventListener("pointerleave", () => { hovering = false; applyPause(); });
    ticker.addEventListener("focusin", () => { focused = true; applyPause(); });
    ticker.addEventListener("focusout", () => { focused = false; applyPause(); });
    let resizeTimer = null;
    window.addEventListener("resize", () => {
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => render(current, true), 150); // re-measure
    });
  }

  // ------------------------------------------------------------------
  // Pages without live rows get their own stream
  // ------------------------------------------------------------------
  window.addEventListener("DOMContentLoaded", () => {
    if (!document.getElementById("nav-badge") || document.getElementById("live-rows")) return;
    const es = new EventSource("/events?view=badge");
    es.addEventListener("badge", (e) => window.updateNavBadge(Number(e.data)));
    es.addEventListener("ticker", (e) => window.updateTicker(e.data));
  });
})();
