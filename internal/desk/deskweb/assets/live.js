// Reloads the page when the desk changes, unless a form is in use.
(function () {
  var main = document.querySelector('main[data-version]');
  if (!main) { return; }
  var seen = main.getAttribute('data-version');
  function busy() {
    var a = document.activeElement;
    if (a && (a.tagName === 'TEXTAREA' || a.tagName === 'INPUT' || a.tagName === 'SELECT')) { return true; }
    var fields = document.querySelectorAll('textarea, input[type=url]');
    for (var i = 0; i < fields.length; i++) { if (fields[i].value) { return true; } }
    return false;
  }
  setInterval(function () {
    fetch('/api/version', { credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (j) {
        if (!j) { return; }
        if (j.v !== seen && !busy()) { location.replace('/'); }
      })
      .catch(function () {});
  }, 3000);
})();
