document.addEventListener("click", function (e) {
  var b = e.target.closest("[data-copy]");
  if (!b) return;
  var pre = document.getElementById(b.dataset.copy);
  if (!navigator.clipboard) {
    var r = document.createRange();
    r.selectNodeContents(pre);
    var s = window.getSelection();
    s.removeAllRanges();
    s.addRange(r);
    return;
  }
  navigator.clipboard.writeText(pre.textContent).then(function () {
    var was = b.textContent;
    b.textContent = "Copied";
    setTimeout(function () { b.textContent = was; }, 1200);
  });
});
