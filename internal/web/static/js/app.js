// Public-page enhancement: a fullscreen lightbox with click-to-zoom for the
// inline images in a recipe's steps. No-op on pages without step images.
(function () {
  "use strict";

  var imgs = document.querySelectorAll(".steps-body img");
  if (!imgs.length) return;

  var overlay = null;
  var overlayImg = null;

  function close() {
    if (overlay) {
      overlay.classList.remove("open");
      document.body.style.overflow = "";
    }
  }

  function open(src) {
    if (!overlay) {
      overlay = document.createElement("div");
      overlay.className = "lightbox";
      overlayImg = document.createElement("img");
      overlay.appendChild(overlayImg);
      overlay.addEventListener("click", function (e) {
        if (e.target === overlayImg) {
          overlayImg.classList.toggle("zoomed");
        } else {
          close();
        }
      });
      document.body.appendChild(overlay);
      document.addEventListener("keydown", function (e) {
        if (e.key === "Escape") close();
      });
    }
    overlayImg.classList.remove("zoomed");
    overlayImg.src = src;
    overlay.classList.add("open");
    document.body.style.overflow = "hidden";
  }

  imgs.forEach(function (img) {
    img.classList.add("zoomable");
    img.setAttribute("tabindex", "0");
    img.addEventListener("click", function () { open(img.src); });
    img.addEventListener("keydown", function (e) {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        open(img.src);
      }
    });
  });
})();
