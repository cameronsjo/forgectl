// Exchanges the key in the address fragment (#k=...) for a session cookie,
// then strips the fragment so it is not left in the address bar or history.
(function () {
  var status = document.getElementById('login-status');
  var m = /(?:^#|&)k=([0-9a-f]{64})(?:&|$)/.exec(location.hash);
  if (!m) { return; }
  history.replaceState(null, '', location.pathname);
  var body = new URLSearchParams();
  body.set('k', m[1]);
  fetch('/session', { method: 'POST', body: body, credentials: 'same-origin' })
    .then(function (r) {
      if (r.status === 204) { location.replace('/'); return; }
      status.textContent = 'That key was not accepted. Run forgectl desk serve again and open the address it prints.';
    })
    .catch(function () { status.textContent = 'Could not reach the desk server.'; });
})();
