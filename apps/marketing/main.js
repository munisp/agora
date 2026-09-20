/* OpenDesk marketing site — vertical filter chips + mobile nav niceties.
   Dependency-free, no network requests. */
(function () {
  "use strict";

  /* Vertical showcase filtering */
  var chips = Array.prototype.slice.call(document.querySelectorAll(".chip[data-filter]"));
  var cards = Array.prototype.slice.call(document.querySelectorAll(".vertical-card[data-group]"));

  function applyFilter(filter) {
    cards.forEach(function (card) {
      var show = filter === "all" || card.getAttribute("data-group") === filter;
      if (show) {
        card.removeAttribute("hidden");
      } else {
        card.setAttribute("hidden", "");
      }
    });
  }

  chips.forEach(function (chip) {
    chip.addEventListener("click", function () {
      chips.forEach(function (c) {
        c.classList.remove("is-active");
        c.setAttribute("aria-pressed", "false");
      });
      chip.classList.add("is-active");
      chip.setAttribute("aria-pressed", "true");
      applyFilter(chip.getAttribute("data-filter"));
    });
  });

  /* Close the mobile <details> nav after tapping a link or pressing Escape */
  var navToggle = document.querySelector(".nav-toggle");
  if (navToggle) {
    navToggle.addEventListener("click", function (event) {
      if (event.target.closest("a")) {
        navToggle.removeAttribute("open");
      }
    });
    document.addEventListener("keydown", function (event) {
      if (event.key === "Escape") {
        navToggle.removeAttribute("open");
      }
    });
  }

  /* SPEC-W45 STK O5: signup CTA → admin-web /signup. This is a static,
   * no-build site, so the app URL is injected at deploy time as a global
   * (window.NEXT_PUBLIC_APP_URL, e.g. via a tiny templated config script);
   * absent that, the CTA falls back to the local-dev default. */
  var appUrl =
    (typeof window.NEXT_PUBLIC_APP_URL === "string" &&
      window.NEXT_PUBLIC_APP_URL.replace(/\/+$/, "")) ||
    "http://localhost:3000";
  document.querySelectorAll("a[data-app-signup]").forEach(function (a) {
    a.setAttribute("href", appUrl + "/signup");
  });
})();
