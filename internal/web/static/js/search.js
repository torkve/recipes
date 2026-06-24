// Catalog enhancement: debounced full-text search and instant category
// filtering without a full page reload. Falls back to plain GET navigation
// (the form and chip links work server-side) when JS or fetch is unavailable.
(function () {
  "use strict";

  var form = document.getElementById("search-form");
  var input = document.getElementById("search-input");
  var catField = document.getElementById("search-cat");
  var nav = document.getElementById("category-nav");
  var results = document.getElementById("results");
  if (!form || !input || !catField || !nav || !results) return;

  var timer = null;

  function buildURL() {
    var params = new URLSearchParams();
    var q = input.value.trim();
    if (q) params.set("q", q);
    if (catField.value) params.set("cat", catField.value);
    var qs = params.toString();
    return "/" + (qs ? "?" + qs : "");
  }

  function fetchResults(push) {
    var url = buildURL();
    fetch(url, { headers: { "X-Requested-With": "fetch" }, credentials: "same-origin" })
      .then(function (r) { return r.text(); })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, "text/html");
        var fresh = doc.getElementById("results");
        if (fresh) results.innerHTML = fresh.innerHTML;
        if (push) history.pushState(null, "", url);
      })
      .catch(function () { form.submit(); });
  }

  function setActiveChip(catValue) {
    nav.querySelectorAll(".chip").forEach(function (c) {
      c.classList.toggle("active", (c.getAttribute("data-cat") || "") === (catValue || ""));
    });
  }

  form.addEventListener("submit", function (e) {
    e.preventDefault();
    fetchResults(true);
  });

  input.addEventListener("input", function () {
    clearTimeout(timer);
    timer = setTimeout(function () { fetchResults(true); }, 250);
  });

  nav.addEventListener("click", function (e) {
    var chip = e.target.closest(".chip");
    if (!chip) return;
    e.preventDefault();
    catField.value = chip.getAttribute("data-cat") || "";
    setActiveChip(catField.value);
    fetchResults(true);
  });

  window.addEventListener("popstate", function () {
    var p = new URLSearchParams(location.search);
    input.value = p.get("q") || "";
    catField.value = p.get("cat") || "";
    setActiveChip(catField.value);
    fetchResults(false);
  });
})();
