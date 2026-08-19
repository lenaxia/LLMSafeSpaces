'use strict';

const pass = (msg) => '<span class="pass">PASS &mdash; ' + msg + '</span>';
const fail = (msg) => '<span class="fail">FAIL &mdash; ' + msg + '</span>';
const out = (id, html) => {
  document.getElementById(id).innerHTML =
    document.getElementById(id).innerHTML.replace(/<span[\s\S]*<\/span>/, html);
};

const norm = (c) => String(c).replace(/\s+/g, '').toLowerCase();
const computedOf = (id) => norm(getComputedStyle(document.getElementById(id)).color);

/* 1. CSSOM control — el.style.x = y on its OWN element. Never blocked by style-src. */
try {
  const el = document.getElementById('p-cssom-target');
  el.style.color = 'rgb(7, 8, 9)';
  out('p-cssom', computedOf(el.id) === 'rgb(7,8,9)'
    ? pass('set and applied via CSSOM')
    : fail('computed ' + computedOf(el.id) + ' != rgb(7,8,9) — methodology broken, distrust style rows'));
} catch (e) { out('p-cssom', fail('threw: ' + e)); }

/* 2. Inline <style> block — was its rule applied? */
try {
  out('p-styleelem', computedOf('p-styleelem-target') === 'rgb(1,2,3)'
    ? pass('inline <style> rule applied')
    : fail('rule not applied (computed ' + computedOf('p-styleelem-target') + ')'));
} catch (e) { out('p-styleelem', fail('threw: ' + e)); }

/* 3. Inline style= attribute — applied? */
try {
  out('p-styleattr', computedOf('p-styleattr-target') === 'rgb(4,5,6)'
    ? pass('inline style attribute applied')
    : fail('attribute not applied (computed ' + computedOf('p-styleattr-target') + ')'));
} catch (e) { out('p-styleattr', fail('threw: ' + e)); }

/* 4. new Function — requires unsafe-eval when CSP present */
try {
  const r = new Function('return 41 + 1')();
  out('p-newfn', r === 42 ? pass('executed (returned ' + r + ')')
                          : fail('returned ' + r));
} catch (e) { out('p-newfn', fail('threw: ' + e.name + ' (CSP eval restriction)')); }

/* 5. eval — same restriction */
try {
  const r = eval('40 + 2');
  out('p-eval', r === 42 ? pass('executed (returned ' + r + ')')
                         : fail('returned ' + r));
} catch (e) { out('p-eval', fail('threw: ' + e.name + ' (CSP eval restriction)')); }

/* 6. application/json script block — intact and parseable? (strip-test for proxies) */
try {
  const raw = document.getElementById('p-json-data').textContent;
  const data = JSON.parse(raw);
  out('p-jsonblk', (data && data.probe === 'tier3' && data.ok === true && data.n === 42)
    ? pass('block present, ' + raw.length + ' bytes, parsed cleanly')
    : fail('parsed but wrong content: ' + raw));
} catch (e) { out('p-jsonblk', fail('missing or unparseable: ' + e)); }

/* 7. Cross-origin CDN script — does it load AND execute? */
(() => {
  let settled = false;
  const settle = (html) => { if (!settled) { settled = true; out('p-cdn', html); } };
  try {
    const s = document.createElement('script');
    s.src = 'https://cdn.jsdelivr.net/npm/jquery@3.7.1/dist/jquery.min.js';
    s.onload = () => {
      if (window.jQuery) settle(pass('loaded + executed (jQuery ' + window.jQuery.fn.jquery + ')'));
      else settle(fail('onload fired but no window.jQuery — loaded without executing?'));
    };
    s.onerror = () => settle(fail('blocked by CSP script-src, or CDN unreachable from your network'));
    document.head.appendChild(s);
    setTimeout(() => settle(fail('timeout after 10s')), 10000);
  } catch (e) { settle(fail('threw: ' + e)); }
})();

/* 8. fetch same-origin */
(() => {
  let settled = false;
  const settle = (html) => { if (!settled) { settled = true; out('p-fetchsame', html); } };
  fetch('fetch-probe.json', { cache: 'no-store' })
    .then(r => r.json())
    .then(d => settle(d && d.ok === true
      ? pass('fetched + parsed (probe=' + d.probe + ')')
      : fail('unexpected body: ' + JSON.stringify(d))))
    .catch(e => settle(fail('threw: ' + e)))
    .finally(() => setTimeout(() => settle(fail('timeout after 8s')), 0));
  setTimeout(() => settle(fail('timeout after 8s')), 8000);
})();

/* 9. fetch cross-origin (CORS-enabled endpoint) */
(() => {
  let settled = false;
  const settle = (html) => { if (!settled) { settled = true; out('p-fetchcross', html); } };
  fetch('https://api.github.com/zen', { mode: 'cors' })
    .then(r => r.text())
    .then(t => settle(pass('fetched cross-origin: "' + t.slice(0, 40) + '"')))
    .catch(e => settle(fail('blocked by connect-src, CORS, or network: ' + e)))
    .finally(() => setTimeout(() => settle(fail('timeout after 8s')), 0));
  setTimeout(() => settle(fail('timeout after 8s')), 8000);
})();
