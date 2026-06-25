// Admin category tree: drag a category onto another to nest it, or onto the
// "top level" zone to detach it. Posts to the existing reparent endpoint and
// reloads to re-render the tree (and show the flash). Rename/delete use plain
// forms and work without this script.
(function () {
  "use strict";

  var tree = document.getElementById("cat-tree");
  if (!tree) return;

  // parentOf maps each category id to its parent id (from data attributes), used
  // to reject dropping a node onto one of its own descendants.
  function parentOf(id) {
    var el = tree.querySelector('.cat-node[data-id="' + id + '"]');
    return el ? el.getAttribute("data-parent") || "" : "";
  }
  function isDescendant(targetID, draggedID) {
    for (var cur = targetID; cur; cur = parentOf(cur)) {
      if (cur === draggedID) return true;
    }
    return false;
  }

  var draggedID = null;

  tree.addEventListener("dragstart", function (e) {
    var node = e.target.closest(".cat-node");
    if (!node) return;
    draggedID = node.getAttribute("data-id");
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData("text/plain", draggedID);
    node.classList.add("dragging");
  });

  tree.addEventListener("dragend", function () {
    draggedID = null;
    tree.querySelectorAll(".dragging, .drag-over").forEach(function (el) {
      el.classList.remove("dragging", "drag-over");
    });
  });

  // dropTarget returns the element a drop would apply to (a node or the root
  // zone), or null when the drop is invalid (onto self or a descendant).
  function dropTarget(e) {
    if (!draggedID) return null;
    var root = e.target.closest(".cat-droproot");
    if (root) return root;
    var node = e.target.closest(".cat-node");
    if (!node) return null;
    var targetID = node.getAttribute("data-id");
    if (targetID === draggedID || isDescendant(targetID, draggedID)) return null;
    return node;
  }

  tree.addEventListener("dragover", function (e) {
    var t = dropTarget(e);
    if (!t) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";
    tree.querySelectorAll(".drag-over").forEach(function (el) { el.classList.remove("drag-over"); });
    t.classList.add("drag-over");
  });

  tree.addEventListener("drop", function (e) {
    var t = dropTarget(e);
    if (!t) return;
    e.preventDefault();
    var id = draggedID;
    var parent = t.classList.contains("cat-droproot") ? "" : t.getAttribute("data-id");
    if (parentOf(id) === parent) return; // no change

    var body = new URLSearchParams();
    body.set("parent_id", parent);
    fetch("/admin/categories/" + encodeURIComponent(id) + "/parent", {
      method: "POST",
      credentials: "same-origin", // sends Sec-Fetch-Site: same-origin → allowed
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: body.toString(),
    }).then(function (r) {
      window.location.href = r.url || "/admin/categories";
    }).catch(function () {
      window.location.href = "/admin/categories";
    });
  });
})();
