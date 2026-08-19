const out = (id, html) => { document.getElementById(id).innerHTML = html; };

// Derive the WS URL from the page URL so the /api/v1/workspaces/.../5173
// prefix is preserved: wss://<host>/api/v1/.../5173/ws
const path = location.pathname.replace(/\/[^/]*$/, '') + '/ws';
const url = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + path;
out('t-url', 'Endpoint: <code>' + url + '</code>');

let ws;
let opened = false;
let done = false;

try {
  ws = new WebSocket(url);
} catch (e) {
  done = true;
  out('t-status', 'Status: <span class="fail">FAIL &mdash; constructor threw: ' + e + '</span>');
}

if (ws) {
  ws.onopen = () => {
    opened = true;
    out('t-status', 'Status: OPEN &mdash; sent echo probe, awaiting reply&hellip;');
    ws.send('hello-from-tunnel');
  };
  ws.onmessage = (m) => {
    done = true;
    out('t-status', m.data === 'hello-from-tunnel'
      ? 'Status: <span class="pass">PASS &mdash; server echoed the message back through the tunnel</span>'
      : 'Status: <span class="fail">FAIL &mdash; unexpected reply: ' + m.data + '</span>');
    ws.close(1000);
  };
  ws.onclose = (e) => {
    if (done) return;
    done = true;
    out('t-status', opened
      ? 'Status: <span class="fail">FAIL &mdash; closed after open, before echo (code ' + e.code + ')</span>'
      : 'Status: <span class="fail">FAIL &mdash; closed before open (code ' + e.code +
        '). Code 1006 = upgrade refused by proxy, or blocked by CSP connect-src</span>');
  };
  ws.onerror = () => {
    if (!done && !opened) {
      out('t-status', 'Status: <span class="fail">FAIL &mdash; error event; connection could not be established</span>');
    }
  };
  setTimeout(() => {
    if (!done) {
      done = true;
      out('t-status', 'Status: <span class="fail">FAIL &mdash; timeout: no echo within 10s</span>');
    }
  }, 10000);
}
