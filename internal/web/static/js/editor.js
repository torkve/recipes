// Recipe form editor: dynamic ingredient blocks, clipboard image upload into
// the steps body, a live preview, and syncing the steps HTML on submit.
(function () {
  "use strict";

  var form = document.getElementById("recipe-form");
  if (!form) return;

  var editor = document.getElementById("steps-editor");
  var hidden = document.getElementById("steps-html");
  var blocks = document.getElementById("ingredient-blocks");
  var tmpl = document.getElementById("ing-block-template");

  // --- Ingredient blocks ---------------------------------------------------
  var addBtn = document.getElementById("add-block");
  if (addBtn) {
    addBtn.addEventListener("click", function () {
      blocks.appendChild(tmpl.content.cloneNode(true));
    });
  }
  blocks.addEventListener("click", function (e) {
    if (!e.target.classList.contains("remove-block")) return;
    var block = e.target.closest(".ing-block");
    if (blocks.querySelectorAll(".ing-block").length > 1) {
      block.remove();
    } else {
      block.querySelector('input[name="ing_subtitle"]').value = "";
      block.querySelector('textarea[name="ing_items"]').value = "";
    }
  });

  // --- Clipboard image upload ---------------------------------------------
  editor.addEventListener("paste", function (e) {
    var items = (e.clipboardData && e.clipboardData.items) || [];
    for (var i = 0; i < items.length; i++) {
      if (items[i].type && items[i].type.indexOf("image/") === 0) {
        e.preventDefault();
        uploadImage(items[i].getAsFile());
      }
    }
  });

  function uploadImage(file) {
    if (!file) return;
    var fd = new FormData();
    fd.append("image", file);
    fetch("/admin/recipes/upload", {
      method: "POST",
      body: fd,
      credentials: "same-origin" // sends Sec-Fetch-Site: same-origin → allowed
    })
      .then(function (r) {
        if (!r.ok) throw new Error("upload failed");
        return r.json();
      })
      .then(function (d) { insertImage(d.url); })
      .catch(function () { alert("Не удалось загрузить изображение"); });
  }

  function insertImage(url) {
    var img = document.createElement("img");
    img.src = url;
    editor.focus();
    var sel = window.getSelection();
    if (sel && sel.rangeCount) {
      var range = sel.getRangeAt(0);
      range.collapse(false);
      range.insertNode(img);
      range.setStartAfter(img);
      range.setEndAfter(img);
      sel.removeAllRanges();
      sel.addRange(range);
    } else {
      editor.appendChild(img);
    }
  }

  // --- Sync steps HTML on submit ------------------------------------------
  form.addEventListener("submit", function () {
    hidden.value = editor.innerHTML;
  });

  // --- Preview -------------------------------------------------------------
  var previewBtn = document.getElementById("preview-btn");
  var preview = document.getElementById("preview");
  var previewBody = document.getElementById("preview-body");

  function escapeHtml(s) {
    var d = document.createElement("div");
    d.textContent = s;
    return d.innerHTML;
  }

  if (previewBtn) {
    previewBtn.addEventListener("click", function () {
      var title = form.querySelector('input[name="title"]').value.trim() || "(без названия)";
      var html = "<h1>" + escapeHtml(title) + "</h1>";

      var ingHtml = "";
      blocks.querySelectorAll(".ing-block").forEach(function (b) {
        var sub = b.querySelector('input[name="ing_subtitle"]').value.trim();
        var items = b.querySelector('textarea[name="ing_items"]').value
          .split("\n").map(function (s) { return s.trim(); }).filter(Boolean);
        if (!sub && items.length === 0) return;
        if (sub) ingHtml += "<h3>" + escapeHtml(sub) + "</h3>";
        if (items.length) {
          ingHtml += "<ul>" + items.map(function (i) { return "<li>" + escapeHtml(i) + "</li>"; }).join("") + "</ul>";
        }
      });
      if (ingHtml) html += "<h2>Ингредиенты</h2>" + ingHtml;
      html += "<h2>Приготовление</h2>" + editor.innerHTML;

      previewBody.innerHTML = html;
      preview.hidden = false;
      preview.scrollIntoView({ behavior: "smooth" });
    });
  }
})();
