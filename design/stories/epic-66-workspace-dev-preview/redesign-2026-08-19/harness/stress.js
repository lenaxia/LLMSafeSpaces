'use strict';

const $ = (id) => document.getElementById(id);
const setStatus = (html) => { $('t-status').innerHTML = html; };
const live = (line) => {
  const el = $('live');
  el.textContent += line + '\n';
  el.scrollTop = el.scrollHeight;
};

const STOP = { flag: false };
$('b-stop').addEventListener('click', () => {
  STOP.flag = true;
  $('b-stop').disabled = true;
  live('>> STOP requested — winding down after in-flight requests');
});

const S = {
  started: new Date().toISOString(),
  budgetInitial: null,
  limitHeader: null,
  sse: null,
  big: null,
  waves: [],
  first429: null,
  used: 0,
  recovery: null,
  notes: [],
};

const pct = (arr, p) => {
  if (!arr.length) return null;
  const s = [...arr].sort((a, b) => a - b);
  return s[Math.min(s.length - 1, Math.floor(p * s.length))];
};

function recordHeaders(h) {
  return {
    remaining: h.get('x-ratelimit-remaining'),
    limit: h.get('x-ratelimit-limit'),
    reset: h.get('x-ratelimit-reset'),
    retryAfter: h.get('retry-after'),
    cfCache: h.get('cf-cache-status'),
    server: h.get('server'),
  };
}

async function probe(i) {
  // 1-byte dynamic response: never cacheable, minimal byte cost.
  const t0 = performance.now();
  S.used++;
  try {
    const r = await fetch('big?bytes=1&i=' + i, { cache: 'no-store' });
    const ms = Math.round(performance.now() - t0);
    return { ok: r.ok, status: r.status, ms, h: recordHeaders(r.headers) };
  } catch (e) {
    return { ok: false, status: 0, ms: Math.round(performance.now() - t0),
             h: null, err: String(e) };
  }
}

async function runWave(n, conc) {
  const results = [];
  let next = 0;
  const worker = async () => {
    while (!STOP.flag && next < n) {
      const r = await probe(next++);
      results.push(r);
      if (r.status === 429 && !S.first429) {
        S.first429 = { requestIndex: S.used, ...r.h };
        live('>> FIRST 429 at request #' + S.used +
             '  retryAfter=' + r.h.retryAfter + '  reset=' + r.h.reset);
      }
    }
  };
  await Promise.all(Array.from({ length: conc }, worker));
  const ok = results.filter(r => r.status === 200);
  const lat = ok.map(r => r.ms);
  const w = {
    n, conc,
    ok: ok.length,
    err429: results.filter(r => r.status === 429).length,
    errOther: results.filter(r => r.status !== 200 && r.status !== 429).length,
    minMs: lat.length ? Math.min(...lat) : null,
    p50Ms: pct(lat, 0.5),
    maxMs: lat.length ? Math.max(...lat) : null,
    lastRemaining: results.length
      ? results[results.length - 1].h && results[results.length - 1].h.remaining
      : null,
    cfCache: [...new Set(results.map(r => r.h && r.h.cfCache).filter(Boolean))],
  };
  S.waves.push(w);
  live('   wave n=' + n + ' c=' + conc + ' -> ok=' + w.ok + ' 429=' + w.err429 +
       ' other=' + w.errOther + ' p50=' + w.p50Ms + 'ms max=' + w.maxMs +
       'ms rem=' + w.lastRemaining + ' cf=' + (w.cfCache.join(',') || '-'));
  return w;
}

function sseTest() {
  return new Promise((resolve) => {
    const t0 = performance.now();
    const arrivals = [];
    let chunks = 0;
    let settled = false;
    const es = new EventSource('sse?d=200&n=10');
    const finish = (mode, err) => {
      if (settled) return;
      settled = true;
      es.close();
      const totalMs = Math.round(performance.now() - t0);
      const gaps = [];
      for (let i = 1; i < arrivals.length; i++) {
        gaps.push(Math.round(arrivals[i] - arrivals[i - 1]));
      }
      S.sse = {
        chunks, totalMs,
        interChunkMs: gaps,
        mode, // 'streamed' | 'buffered' | 'error'
        err: err || null,
      };
      live('   SSE: ' + chunks + ' chunks in ' + totalMs + 'ms, gaps=[' +
           gaps.join(',') + '] -> ' + mode + (err ? ' (' + err + ')' : ''));
      resolve();
    };
    es.onmessage = (m) => {
      arrivals.push(performance.now());
      chunks++;
      try {
        const d = JSON.parse(m.data);
        if (typeof d.i === 'number' && d.i === 0 && chunks === 1 && arrivals[0] - t0 > 1500) {
          // first chunk arrived late relative to drip cadence — suspicious of buffering
        }
      } catch (e) { /* ignore */ }
    };
    es.addEventListener('done', () => {
      // Buffered proxy would deliver everything near-instantly at close;
      // streamed arrives every ~200ms over ~2s.
      const spread = arrivals.length > 2
        ? arrivals[arrivals.length - 1] - arrivals[0] : 0;
      finish(spread > 1200 ? 'streamed' : 'buffered');
    });
    es.onerror = (e) => {
      if (chunks === 0) finish('error', 'no events received');
      else finish(arrivals.length > 2 &&
        (arrivals[arrivals.length - 1] - arrivals[0]) > 1200 ? 'streamed' : 'buffered');
    };
    setTimeout(() => finish('error', 'timeout 30s'), 30000);
  });
}

async function bigTest(bytes) {
  const t0 = performance.now();
  S.used++;
  try {
    const r = await fetch('big?bytes=' + bytes, { cache: 'no-store' });
    const buf = await r.arrayBuffer();
    const ms = Math.round(performance.now() - t0);
    S.big = {
      requested: bytes, received: buf.byteLength, status: r.status, ms,
      mbps: +(buf.byteLength / 1024 / 1024 / (ms / 1000)).toFixed(2),
      cfCache: r.headers.get('cf-cache-status'),
    };
    live('   BIG: ' + buf.byteLength + '/' + bytes + ' bytes, ' + ms + 'ms (' +
         S.big.mbps + ' MB/s), status=' + r.status + ', cf=' + S.big.cfCache);
  } catch (e) {
    S.big = { requested: bytes, error: String(e) };
    live('   BIG: FAILED ' + e);
  }
}

async function main() {
  $('b-start').disabled = true;
  $('b-stop').disabled = false;
  try {
    // Phase 0: budget read
    live('== phase 0: budget read ==');
    const r = await fetch(location.pathname, { cache: 'no-store' });
    S.used++;
    const h = recordHeaders(r.headers);
    S.limitHeader = h.limit;
    S.budgetInitial = h.remaining;
    $('t-budget').innerHTML = 'Rate budget: limit=<strong>' + h.limit +
      '</strong> remaining=<strong>' + h.remaining + '</strong> reset=' + h.reset +
      ' cf=' + h.cfCache;
    live('   limit=' + h.limit + ' remaining=' + h.remaining + ' reset=' + h.reset);

    // Phase 1: SSE streaming (1 request)
    live('== phase 1: SSE drip (10 chunks @ 200ms) ==');
    await sseTest();

    // Phase 2: 2MB transfer (1 request)
    live('== phase 2: 2MB body ==');
    await bigTest(2 * 1024 * 1024);

    if (STOP.flag) throw new Error('stopped by user');

    // Phase 3: waves until 429 or cap
    live('== phase 3: request waves (cap 120 total) ==');
    const waves = [[5, 1], [10, 2], [20, 5], [60, 10]];
    for (const [n, c] of waves) {
      if (STOP.flag || S.first429 || S.used >= 120) break;
      live('   wave: n=' + n + ' conc=' + c);
      await runWave(n, c);
    }

    // Phase 4: drain until 429 if not seen yet (hard cap 120)
    if (!S.first429 && !STOP.flag && S.used < 120) {
      live('== phase 4: drain at conc=10 until 429 ==');
      while (!STOP.flag && !S.first429 && S.used < 120) {
        await runWave(10, 10);
      }
    }

    // Phase 5: recovery timing
    if ((S.first429 || S.budgetInitial === '0') && !STOP.flag) {
      live('== phase 5: recovery probe (1 req / 10s, max 5min) ==');
      const t0 = Date.now();
      const samples = [];
      for (let i = 0; i < 30 && !STOP.flag; i++) {
        await new Promise(res => setTimeout(res, i === 0 ? 1000 : 10000));
        const r = await probe('rec' + i);
        samples.push({ s: Math.round((Date.now() - t0) / 1000),
                       status: r.status, remaining: r.h && r.h.remaining });
        live('   +' + samples[i].s + 's status=' + r.status +
             ' remaining=' + samples[i].remaining);
        if (r.status === 200 && r.h && Number(r.h.remaining) >= 10) break;
      }
      const firstOk = samples.find(x => x.status === 200);
      S.recovery = {
        secondsToFirst200: firstOk ? firstOk.s : null,
        samples,
      };
    } else if (!S.first429) {
      S.notes.push('no 429 observed within cap — budget larger than 120 requests or not enforced on subresources');
    }
  } catch (e) {
    S.notes.push('aborted: ' + e);
  }

  S.finished = new Date().toISOString();
  S.totalRequests = S.used;
  const out = JSON.stringify(S, null, 2);
  $('summary').textContent = out;
  setStatus('Done. ' + S.used + ' requests used. Summary below &mdash; paste it back to the agent.');
  $('b-stop').disabled = true;
  live('== COMPLETE ==');
}

$('b-start').addEventListener('click', main);
