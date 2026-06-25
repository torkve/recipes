// Catalog enhancement: debounced full-text search and instant category
// filtering (dropdown + breadcrumbs) without a full page reload. Falls back to
// plain GET navigation (the form and category links work server-side) when JS or
// fetch is unavailable.
(function () {
  "use strict";

  var form = document.getElementById("search-form");
  var input = document.getElementById("search-input");
  var catField = document.getElementById("search-cat");
  var dropdown = document.getElementById("category-nav");
  var breadcrumbs = document.getElementById("breadcrumbs");
  var results = document.getElementById("results");
  if (!form || !input || !catField || !results) return;

  var timer = null;

  function buildURL() {
    var params = new URLSearchParams();
    var q = input.value.trim();
    if (q) params.set("q", q);
    if (catField.value) params.set("cat", catField.value);
    var qs = params.toString();
    return "/" + (qs ? "?" + qs : "");
  }

  function setActive(catValue) {
    if (!dropdown) return;
    dropdown.querySelectorAll(".cat-item").forEach(function (c) {
      c.classList.toggle("active", (c.getAttribute("data-cat") || "") === (catValue || ""));
    });
  }

  function fetchResults(push) {
    var url = buildURL();
    fetch(url, { headers: { "X-Requested-With": "fetch" }, credentials: "same-origin" })
      .then(function (r) { return r.text(); })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, "text/html");
        var fresh = doc.getElementById("results");
        if (fresh) results.innerHTML = fresh.innerHTML;
        var crumbs = doc.getElementById("breadcrumbs");
        if (breadcrumbs && crumbs) breadcrumbs.innerHTML = crumbs.innerHTML;
        setActive(catField.value);
        if (push) history.pushState(null, "", url);
      })
      .catch(function () { form.submit(); });
  }

  form.addEventListener("submit", function (e) {
    e.preventDefault();
    fetchResults(true);
  });

  input.addEventListener("input", function () {
    clearTimeout(timer);
    timer = setTimeout(function () { fetchResults(true); }, 250);
  });

  // Category links live in both the dropdown and the breadcrumbs (the latter is
  // re-rendered on each fetch), so delegate from the document.
  document.addEventListener("click", function (e) {
    var link = e.target.closest("a[data-cat]");
    if (!link) return;
    if ((dropdown && dropdown.contains(link)) || (breadcrumbs && breadcrumbs.contains(link))) {
      e.preventDefault();
      catField.value = link.getAttribute("data-cat") || "";
      setActive(catField.value);
      if (dropdown) dropdown.open = false; // close the <details> after choosing
      fetchResults(true);
    }
  });

  window.addEventListener("popstate", function () {
    var p = new URLSearchParams(location.search);
    input.value = p.get("q") || "";
    catField.value = p.get("cat") || "";
    fetchResults(false);
  });
})();
