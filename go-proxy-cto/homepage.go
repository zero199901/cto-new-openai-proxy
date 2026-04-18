package main

// HomepageHTML 访问 / 时展示的完整 Dashboard
const HomepageHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>cto.new 反代 Dashboard</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,"SF Pro Display","Segoe UI",sans-serif;background:#09090b;color:#fafafa;min-height:100vh}
.container{max-width:1200px;margin:0 auto;padding:24px 20px}
header{display:flex;align-items:center;justify-content:space-between;padding:12px 0 24px;flex-wrap:wrap;gap:12px}
header h1{font-size:1.8rem;font-weight:800;letter-spacing:-0.5px;background:linear-gradient(135deg,#3b82f6,#8b5cf6);-webkit-background-clip:text;-webkit-text-fill-color:transparent}
header .meta{display:flex;align-items:center;gap:12px;font-size:.85rem;color:#71717a}
.pulse{width:8px;height:8px;border-radius:50%;background:#22c55e;display:inline-block;animation:blink 2s infinite}
@keyframes blink{0%,100%{opacity:1}50%{opacity:.3}}
.tabs{display:flex;gap:4px;margin-bottom:20px;background:#18181b;border-radius:12px;padding:4px;width:fit-content}
.tab{padding:8px 20px;border-radius:8px;cursor:pointer;font-size:.9rem;font-weight:600;color:#a1a1aa;transition:all .15s}
.tab:hover{color:#e4e4e7}
.tab.active{background:#27272a;color:#fafafa}
.card{background:#18181b;border:1px solid #27272a;border-radius:16px;padding:24px;margin-bottom:20px}
.card h2{font-size:1.1rem;font-weight:700;margin-bottom:16px;display:flex;align-items:center;gap:8px}
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(110px,1fr));gap:10px}
.stat{background:rgba(39,39,42,.6);border-radius:12px;padding:14px 10px;text-align:center}
.stat .v{font-size:1.5rem;font-weight:800;color:#3b82f6}
.stat .l{font-size:.7rem;color:#71717a;margin-top:4px;text-transform:uppercase;letter-spacing:.5px}
.stat.green .v{color:#22c55e}
.stat.red .v{color:#ef4444}
.stat.yellow .v{color:#eab308}
.stat.purple .v{color:#a78bfa}
table{width:100%;border-collapse:collapse;font-size:.85rem}
th{text-align:left;padding:10px 12px;color:#71717a;font-weight:600;border-bottom:1px solid #27272a;text-transform:uppercase;font-size:.72rem;letter-spacing:.5px}
td{padding:10px 12px;border-bottom:1px solid rgba(39,39,42,.5);color:#d4d4d8}
tr:hover td{background:rgba(39,39,42,.3)}
.badge{display:inline-block;padding:2px 8px;border-radius:6px;font-size:.75rem;font-weight:600}
.badge.ok{background:rgba(34,197,94,.15);color:#4ade80}
.badge.err{background:rgba(239,68,68,.15);color:#f87171}
.badge.warn{background:rgba(234,179,8,.15);color:#facc15}
.badge.used{background:rgba(59,130,246,.15);color:#60a5fa}
.log-row{font-family:"SF Mono",Menlo,monospace;font-size:.78rem}
.log-row .time{color:#71717a;white-space:nowrap}
.log-row .method{color:#a78bfa;font-weight:600}
.log-row .path{color:#93c5fd}
.log-row .model{color:#fbbf24}
.log-row .dur{color:#71717a;text-align:right}
.log-row .status-ok{color:#4ade80}
.log-row .status-err{color:#f87171}
.log-row .account{color:#71717a;max-width:160px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.log-scroll{max-height:420px;overflow-y:auto;border-radius:8px}
.log-scroll::-webkit-scrollbar{width:6px}
.log-scroll::-webkit-scrollbar-track{background:transparent}
.log-scroll::-webkit-scrollbar-thumb{background:#27272a;border-radius:3px}
.models{display:flex;flex-wrap:wrap;gap:8px;margin-top:8px}
.chip{background:rgba(59,130,246,.12);color:#93c5fd;border:1px solid rgba(59,130,246,.25);padding:4px 10px;border-radius:8px;font-size:.8rem;font-weight:500}
pre{background:#0a0a0c;border-radius:10px;padding:14px;color:#d4d4d8;font-size:.82rem;overflow-x:auto;line-height:1.5}
.section{display:none}
.section.active{display:block}
.empty{text-align:center;color:#52525b;padding:40px 0;font-size:.9rem}
.actions{display:flex;gap:8px;margin-top:12px}
.btn{padding:8px 16px;border-radius:8px;border:1px solid #27272a;background:#27272a;color:#e4e4e7;font-size:.82rem;cursor:pointer;font-weight:500;transition:all .15s}
.btn:hover{background:#3f3f46}
.btn.primary{background:#3b82f6;border-color:#3b82f6;color:#fff}
.btn.primary:hover{background:#2563eb}
</style>
</head>
<body>
<div class="container">
  <header>
    <h1>cto.new 反代</h1>
    <div class="meta">
      <span class="pulse"></span>
      <span id="uptime">Uptime: --</span>
      <span>Listen: 127.0.0.1:9091</span>
    </div>
  </header>

  <div class="tabs">
    <div class="tab active" data-tab="overview">Overview</div>
    <div class="tab" data-tab="accounts">Accounts</div>
    <div class="tab" data-tab="logs">Request Logs</div>
    <div class="tab" data-tab="usage">Usage</div>
  </div>

  <!-- Overview -->
  <div class="section active" id="sec-overview">
    <div class="card">
      <h2>Pool Status</h2>
      <div class="stats" id="stats">
        <div class="stat"><div class="v" id="total">-</div><div class="l">Total</div></div>
        <div class="stat green"><div class="v" id="available">-</div><div class="l">Available</div></div>
        <div class="stat yellow"><div class="v" id="exhausted">-</div><div class="l">Exhausted</div></div>
        <div class="stat red"><div class="v" id="dead">-</div><div class="l">Dead</div></div>
        <div class="stat used"><div class="v" id="in_use">-</div><div class="l">In Use</div></div>
        <div class="stat purple"><div class="v" id="total_success">-</div><div class="l">Success</div></div>
      </div>
    </div>
    <div class="card">
      <h2>Request Stats (24h)</h2>
      <div class="stats" id="log-stats">
        <div class="stat"><div class="v" id="ls_total">-</div><div class="l">Requests</div></div>
        <div class="stat green"><div class="v" id="ls_success">-</div><div class="l">Success</div></div>
        <div class="stat red"><div class="v" id="ls_fail">-</div><div class="l">Failed</div></div>
        <div class="stat"><div class="v" id="ls_avg_dur">-</div><div class="l">Avg Duration</div></div>
        <div class="stat purple"><div class="v" id="ls_tokens_in">-</div><div class="l">Tokens In</div></div>
        <div class="stat purple"><div class="v" id="ls_tokens_out">-</div><div class="l">Tokens Out</div></div>
      </div>
    </div>
    <div class="card">
      <h2>Models</h2>
      <div class="models">
        <span class="chip">gpt-5.4</span>
        <span class="chip">glm-5.1</span>
        <span class="chip">gpt-4o / gpt-4 → gpt-5.4</span>
        <span class="chip">claude-sonnet-4.6 → gpt-5.4</span>
        <span class="chip">o1 / o3 → gpt-5.4</span>
      </div>
    </div>
    <div class="card">
      <h2>Quick Start</h2>
<pre>
# OpenAI
curl -N http://127.0.0.1:9091/v1/chat/completions \
  -H 'Authorization: Bearer xs2-cto-secret' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}'

# Anthropic
curl -N http://127.0.0.1:9091/v1/messages \
  -H 'x-api-key: xs2-cto-secret' -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-5.1","stream":true,"max_tokens":1024,
       "messages":[{"role":"user","content":"hi"}]}'
</pre>
    </div>
  </div>

  <!-- Accounts -->
  <div class="section" id="sec-accounts">
    <div class="card">
      <h2>Account Pool <span style="font-weight:400;font-size:.8rem;color:#71717a" id="acc-count"></span></h2>
      <div class="actions">
        <button class="btn primary" onclick="reloadAccounts()">Reload Pool</button>
      </div>
      <div style="overflow-x:auto;margin-top:16px">
        <table>
          <thead><tr><th>#</th><th>Email</th><th>Status</th><th>In Use</th><th>Success</th><th>Errors</th></tr></thead>
          <tbody id="acc-tbody"></tbody>
        </table>
      </div>
    </div>
  </div>

  <!-- Request Logs -->
  <div class="section" id="sec-logs">
    <div class="card">
      <h2>Recent Requests <span style="font-weight:400;font-size:.8rem;color:#71717a" id="log-count"></span></h2>
      <div class="actions">
        <button class="btn" onclick="loadLogs()">Refresh</button>
        <button class="btn" onclick="clearLogView()">Clear View</button>
      </div>
      <div class="log-scroll" id="log-scroll" style="margin-top:16px">
        <table>
          <thead><tr><th>Time</th><th>Method</th><th>Path</th><th>Model</th><th>Account</th><th>Status</th><th>Duration</th><th>Stream</th><th>Error</th></tr></thead>
          <tbody id="log-tbody"></tbody>
        </table>
      </div>
    </div>
  </div>

  <!-- Usage -->
  <div class="section" id="sec-usage">
    <div class="card">
      <h2>Token Usage (24h)</h2>
      <div class="stats" id="usage-stats">
        <div class="stat"><div class="v" id="us_in">-</div><div class="l">Input Tokens</div></div>
        <div class="stat"><div class="v" id="us_out">-</div><div class="l">Output Tokens</div></div>
        <div class="stat"><div class="v" id="us_total">-</div><div class="l">Total Tokens</div></div>
      </div>
    </div>
    <div class="card">
      <h2>Per-Account Usage</h2>
      <div style="overflow-x:auto">
        <table>
          <thead><tr><th>Email</th><th>Success</th><th>Errors</th><th>Status</th></tr></thead>
          <tbody id="usage-tbody"></tbody>
        </table>
      </div>
    </div>
  </div>
</div>

<script>
const API_KEY = 'xs2-cto-secret';
const headers = { 'Authorization': 'Bearer ' + API_KEY };

// Tab switching
document.querySelectorAll('.tab').forEach(t => {
  t.addEventListener('click', () => {
    document.querySelectorAll('.tab').forEach(x => x.classList.remove('active'));
    document.querySelectorAll('.section').forEach(x => x.classList.remove('active'));
    t.classList.add('active');
    document.getElementById('sec-' + t.dataset.tab).classList.add('active');
    if (t.dataset.tab === 'accounts') loadAccounts();
    if (t.dataset.tab === 'logs') loadLogs();
    if (t.dataset.tab === 'usage') loadUsage();
  });
});

function fmtDur(ms) {
  if (ms < 1000) return ms + 'ms';
  return (ms/1000).toFixed(1) + 's';
}

function fmtNum(n) {
  if (n >= 1000000) return (n/1000000).toFixed(1) + 'M';
  if (n >= 1000) return (n/1000).toFixed(1) + 'K';
  return String(n);
}

function fmtTime(s) {
  const d = new Date(s);
  return d.toLocaleTimeString('zh-CN', {hour12:false, hour:'2-digit', minute:'2-digit', second:'2-digit'});
}

function statusBadge(acc) {
  if (acc.dead) return '<span class="badge err">Dead</span>';
  if (acc.exhausted) return '<span class="badge warn">Exhausted</span>';
  if (acc.in_use) return '<span class="badge used">In Use</span>';
  return '<span class="badge ok">Available</span>';
}

async function refreshStatus() {
  try {
    const r = await fetch('/status');
    const d = await r.json();
    for (const k of ['total','available','exhausted','dead','in_use','total_success']) {
      const el = document.getElementById(k);
      if (el && d[k] !== undefined) el.textContent = d[k];
    }
    if (d.uptime_sec !== undefined) {
      const s = d.uptime_sec;
      const h = Math.floor(s/3600);
      const m = Math.floor((s%3600)/60);
      document.getElementById('uptime').textContent = 'Uptime: ' + h + 'h ' + m + 'm';
    }
  } catch(e) {}
}

async function refreshLogStats() {
  try {
    const r = await fetch('/admin/log-stats', {headers});
    const d = await r.json();
    document.getElementById('ls_total').textContent = d.total_24h || 0;
    document.getElementById('ls_success').textContent = d.success_24h || 0;
    document.getElementById('ls_fail').textContent = d.fail_24h || 0;
    document.getElementById('ls_avg_dur').textContent = fmtDur(d.avg_duration_ms || 0);
    document.getElementById('ls_tokens_in').textContent = fmtNum(d.tokens_in_24h || 0);
    document.getElementById('ls_tokens_out').textContent = fmtNum(d.tokens_out_24h || 0);
  } catch(e) {}
}

async function loadAccounts() {
  try {
    const r = await fetch('/admin/accounts', {headers});
    const d = await r.json();
    const accs = d.accounts || [];
    document.getElementById('acc-count').textContent = '(' + accs.length + ' accounts)';
    const tbody = document.getElementById('acc-tbody');
    tbody.innerHTML = accs.map((a, i) =>
      '<tr><td>' + (i+1) + '</td><td style="max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + a.email + '</td><td>' + statusBadge(a) + '</td><td>' + (a.in_use ? 'Yes' : 'No') + '</td><td>' + a.success + '</td><td>' + a.errors + '</td></tr>'
    ).join('');
  } catch(e) {}
}

async function reloadAccounts() {
  try {
    await fetch('/admin/reload', {headers, method:'POST'});
    loadAccounts();
  } catch(e) {}
}

async function loadLogs() {
  try {
    const r = await fetch('/admin/logs?n=200', {headers});
    const d = await r.json();
    const logs = d.logs || [];
    document.getElementById('log-count').textContent = '(' + logs.length + ' entries)';
    const tbody = document.getElementById('log-tbody');
    tbody.innerHTML = logs.map(l => {
      const sc = (l.status >= 200 && l.status < 300) ? 'status-ok' : 'status-err';
      return '<tr class="log-row"><td class="time">' + fmtTime(l.time) + '</td><td class="method">' + l.method + '</td><td class="path">' + l.path + '</td><td class="model">' + (l.model||'-') + '</td><td class="account" title="' + (l.account||'') + '">' + (l.account||'-') + '</td><td class="' + sc + '">' + l.status + '</td><td class="dur">' + fmtDur(l.duration) + '</td><td>' + (l.stream?'S':'-') + '</td><td style="max-width:200px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:#f87171">' + (l.error||'') + '</td></tr>';
    }).join('');
  } catch(e) {}
}

function clearLogView() {
  document.getElementById('log-tbody').innerHTML = '';
  document.getElementById('log-count').textContent = '';
}

async function loadUsage() {
  try {
    const [accR, logR] = await Promise.all([
      fetch('/admin/accounts', {headers}),
      fetch('/admin/log-stats', {headers})
    ]);
    const accD = await accR.json();
    const logD = await logR.json();
    document.getElementById('us_in').textContent = fmtNum(logD.tokens_in_24h || 0);
    document.getElementById('us_out').textContent = fmtNum(logD.tokens_out_24h || 0);
    document.getElementById('us_total').textContent = fmtNum((logD.tokens_in_24h||0) + (logD.tokens_out_24h||0));
    const accs = accD.accounts || [];
    const tbody = document.getElementById('usage-tbody');
    tbody.innerHTML = accs.map(a =>
      '<tr><td style="max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + a.email + '</td><td>' + a.success + '</td><td>' + a.errors + '</td><td>' + statusBadge(a) + '</td></tr>'
    ).join('');
  } catch(e) {}
}

refreshStatus();
refreshLogStats();
setInterval(refreshStatus, 5000);
setInterval(refreshLogStats, 10000);
</script>
</body>
</html>`
