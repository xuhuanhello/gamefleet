// GameFleet console: copy buttons next to IDs, a confirmation before changes
// that matter, and a quiet refresh of pages that list live state. Every page
// works without this script.
(() => {
  const copyIcon = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true">' +
    '<rect x="5.5" y="5.5" width="8" height="8" rx="1.5"/><path d="M10.5 5.5V4A1.5 1.5 0 0 0 9 2.5H4A1.5 1.5 0 0 0 2.5 4v5A1.5 1.5 0 0 0 4 10.5h1.5"/></svg>';
  const doneIcon = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true">' +
    '<path d="M3.5 8.5l3 3 6-7"/></svg>';

  // Elements with data-copy="text" get a button that copies the text.
  function addCopyButtons(root) {
    for (const el of root.querySelectorAll("[data-copy]")) {
      if (el.querySelector(".copy")) continue;
      const button = document.createElement("button");
      button.type = "button";
      button.className = "copy";
      button.title = "复制";
      button.setAttribute("aria-label", "复制 " + el.dataset.copy);
      button.innerHTML = copyIcon;
      button.addEventListener("click", async (event) => {
        event.preventDefault();
        event.stopPropagation();
        try {
          await navigator.clipboard.writeText(el.dataset.copy);
        } catch {
          return;
        }
        button.classList.add("done");
        button.innerHTML = doneIcon;
        setTimeout(() => {
          button.classList.remove("done");
          button.innerHTML = copyIcon;
        }, 1200);
      });
      el.classList.add("copyable");
      el.append(button);
    }
  }

  // <form data-confirm="question"> asks before it submits, and
  // <form data-busy="text"> shows the text on its button while the next page
  // loads.
  document.addEventListener("submit", (event) => {
    const form = event.target;
    const question = form.dataset && form.dataset.confirm;
    if (question && !confirm(question)) {
      event.preventDefault();
      return;
    }
    const button = event.submitter;
    if (form.dataset && form.dataset.busy && button) {
      button.dataset.label = button.textContent;
      button.textContent = form.dataset.busy;
      button.disabled = true;
    }
  });
  // Coming back to a page from the history gives busy buttons their label back.
  addEventListener("pageshow", (event) => {
    if (!event.persisted) return;
    for (const button of document.querySelectorAll("button[data-label]")) {
      button.textContent = button.dataset.label;
      button.disabled = false;
    }
  });

  // edited reports whether a form field in root differs from what the page loaded with.
  function edited(root) {
    for (const el of root.querySelectorAll("input, select, textarea")) {
      if (el.type === "hidden") continue;
      if (el.type === "checkbox" || el.type === "radio" ? el.checked !== el.defaultChecked : el.value !== el.defaultValue) return true;
      if (el.tagName === "SELECT" && [...el.options].some((o) => o.selected !== o.defaultSelected)) return true;
    }
    return false;
  }

  // <main data-refresh="10"> reloads its content every ten seconds while the
  // tab is visible and the reader is not typing, editing a form or selecting
  // text in it.
  function refresh(main) {
    const seconds = Number(main.dataset.refresh);
    if (!seconds) return;
    let shown = main.innerHTML;
    setInterval(async () => {
      const active = document.activeElement;
      if (document.hidden || String(getSelection()) || edited(main) ||
          (active && main.contains(active) && active.matches("input, select, textarea"))) return;
      let response;
      try {
        response = await fetch(location.href, { credentials: "same-origin", headers: { "X-GameFleet-Refresh": "1" } });
      } catch {
        return;
      }
      if (response.redirected && new URL(response.url).pathname === "/login") {
        location.reload();
        return;
      }
      if (!response.ok) return;
      const next = new DOMParser().parseFromString(await response.text(), "text/html").querySelector("main");
      if (!next || next.innerHTML === shown) return;
      shown = next.innerHTML;
      const swap = () => {
        main.innerHTML = shown;
        addCopyButtons(main);
      };
      if (document.startViewTransition && !matchMedia("(prefers-reduced-motion: reduce)").matches) {
        // A transition the browser skips still swaps the content; its
        // promises only report the skip.
        const transition = document.startViewTransition(swap);
        transition.ready.catch(() => {});
        transition.finished.catch(() => {});
      } else {
        swap();
      }
    }, seconds * 1000);
  }

  // <div data-await-version="2.1.0" data-from="2.0.0"> waits while the
  // updater restarts GameFleet: it asks /healthz, which needs no session,
  // until the new version answers, or the old one again after GameFleet was
  // down, which means the updater went back. Then it says how it went, in
  // the page's own words (data-ok, data-failed), and reloads after ten seconds.
  function awaitVersion(el) {
    const want = el.dataset.awaitVersion, from = el.dataset.from, started = Date.now();
    let wentDown = false;
    const text = el.querySelector(".text"), button = el.querySelector("[data-reload]");
    button.addEventListener("click", () => location.reload());
    const poll = setInterval(async () => {
      let version = "";
      try {
        const response = await fetch("/healthz", { cache: "no-store" });
        if (response.ok) version = (await response.json()).version || "";
      } catch {}
      if (!version) wentDown = true;
      if (version === want || (wentDown && version === from)) {
        clearInterval(poll);
        done(version === want);
      } else if (Date.now() - started > 5 * 60 * 1000) {
        clearInterval(poll);
        text.textContent = el.dataset.late;
        button.hidden = false;
      }
    }, 2000);
    function done(ok) {
      const running = document.querySelector("[data-running]");
      if (running) running.textContent = "v" + (ok ? want : from);
      const finding = document.querySelector(".finding");
      if (finding) {
        finding.className = "finding " + (ok ? "ok" : "bad");
        finding.querySelector(".text").textContent = ok ? el.dataset.ok : el.dataset.failed;
      }
      const step = el.closest(".progress-card").querySelector("ol.stepper li.now");
      if (step) {
        step.className = ok ? "done" : "failed";
        const check = el.closest(".progress-card").querySelector("ol.stepper li.done .mark");
        step.querySelector(".mark").innerHTML = ok && check ? check.innerHTML : "!";
      }
      button.hidden = false;
      let left = 10;
      const tick = () => {
        if (left === 0) return location.reload();
        text.textContent = el.dataset.countdown.replace("{n}", left--);
        setTimeout(tick, 1000);
      };
      tick();
    }
  }

  const start = () => {
    const main = document.querySelector("main");
    if (main) refresh(main);
    addCopyButtons(document);
    for (const el of document.querySelectorAll("[data-await-version]")) awaitVersion(el);
  };
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
})();
