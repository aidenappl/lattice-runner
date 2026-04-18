package web

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Lattice Runner</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { background: #0a0a0a; color: #ededed; font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; padding: 24px; max-width: 1200px; margin: 0 auto; }
  .header { display: flex; align-items: center; gap: 12px; margin-bottom: 24px; }
  .header .logo { width: 36px; height: 36px; background: #3b82f6; border-radius: 8px; display: flex; align-items: center; justify-content: center; flex-shrink: 0; }
  .header .logo svg { width: 22px; height: 22px; color: white; }
  .header h1 { font-size: 18px; font-weight: 600; }
  .header h1 span { color: #555; font-weight: 400; margin-left: 6px; font-size: 13px; }
  .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: 12px; margin-bottom: 24px; }
  .card { background: #111; border: 1px solid #1a1a1a; border-radius: 12px; padding: 16px; }
  .card .label { font-size: 10px; color: #888; text-transform: uppercase; letter-spacing: 0.05em; margin-bottom: 8px; }
  .card .value { font-size: 20px; font-weight: 600; }
  .card .sub { font-size: 11px; color: #555; margin-top: 4px; }
  .card .bar { height: 4px; background: #1a1a1a; border-radius: 2px; margin-top: 8px; overflow: hidden; }
  .card .bar .fill { height: 100%; border-radius: 2px; transition: width 0.5s ease; }
  .blue { color: #3b82f6; } .purple { color: #a855f7; } .green { color: #22c55e; } .yellow { color: #eab308; } .red { color: #ef4444; } .gray { color: #888; }
  .section { margin-bottom: 24px; }
  .section h2 { font-size: 13px; font-weight: 500; color: #888; text-transform: uppercase; letter-spacing: 0.05em; margin-bottom: 12px; }
  table { width: 100%; border-collapse: collapse; background: #111; border: 1px solid #1a1a1a; border-radius: 12px; overflow: hidden; }
  th { text-align: left; padding: 10px 14px; font-size: 10px; color: #888; text-transform: uppercase; letter-spacing: 0.05em; border-bottom: 1px solid #1a1a1a; font-weight: 500; }
  td { padding: 10px 14px; font-size: 13px; border-bottom: 1px solid #1a1a1a; }
  tr:last-child td { border-bottom: none; }
  tr:hover td { background: #161616; }
  .mono { font-family: 'SF Mono', 'Fira Code', monospace; font-size: 12px; }
  .dot { display: inline-block; width: 6px; height: 6px; border-radius: 50%; margin-right: 6px; }
  .dot.running { background: #22c55e; } .dot.exited { background: #ef4444; } .dot.created { background: #eab308; } .dot.paused { background: #888; }
  .logs-container { background: #111; border: 1px solid #1a1a1a; border-radius: 12px; overflow: hidden; }
  .logs-header { display: flex; align-items: center; justify-content: space-between; padding: 10px 14px; border-bottom: 1px solid #1a1a1a; }
  .logs-header select { background: #161616; border: 1px solid #2a2a2a; color: #ededed; padding: 4px 8px; border-radius: 6px; font-size: 12px; cursor: pointer; }
  .logs { padding: 14px; font-family: 'SF Mono', 'Fira Code', monospace; font-size: 11px; color: #888; white-space: pre-wrap; word-break: break-all; max-height: 400px; overflow-y: auto; line-height: 1.5; }
  .info-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 0; }
  .info-item { display: flex; justify-content: space-between; padding: 8px 14px; border-bottom: 1px solid #1a1a1a; }
  .info-item:last-child { border-bottom: none; }
  .info-item .k { color: #555; font-size: 12px; } .info-item .v { color: #ededed; font-size: 12px; }
  .empty { color: #555; font-size: 13px; text-align: center; padding: 32px; }
  .two-col { display: grid; grid-template-columns: 1fr 1fr; gap: 24px; }
  @media (max-width: 768px) { .two-col { grid-template-columns: 1fr; } }
</style>
</head>
<body>
  <div class="header">
    <div class="logo">
      <svg fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
        <path stroke-linecap="round" stroke-linejoin="round" d="M4 6a2 2 0 012-2h2a2 2 0 012 2v2a2 2 0 01-2 2H6a2 2 0 01-2-2V6zM14 6a2 2 0 012-2h2a2 2 0 012 2v2a2 2 0 01-2 2h-2a2 2 0 01-2-2V6zM4 16a2 2 0 012-2h2a2 2 0 012 2v2a2 2 0 01-2 2H6a2 2 0 01-2-2v-2zM14 16a2 2 0 012-2h2a2 2 0 012 2v2a2 2 0 01-2 2h-2a2 2 0 01-2-2v-2z"/>
      </svg>
    </div>
    <h1 id="worker-name">Lattice Runner</h1>
  </div>

  <div class="grid" id="metrics"></div>

  <div class="two-col">
    <div class="section">
      <h2>System Info</h2>
      <div class="card" style="padding:0">
        <div class="info-grid" id="info"></div>
      </div>
    </div>
    <div class="section">
      <h2>Load &amp; Resources</h2>
      <div class="card" style="padding:0">
        <div class="info-grid" id="resources"></div>
      </div>
    </div>
  </div>

  <div class="section">
    <h2>Containers</h2>
    <table>
      <thead><tr><th>Name</th><th>Image</th><th>State</th><th>Status</th><th>ID</th></tr></thead>
      <tbody id="containers"><tr><td colspan="5" class="empty">Loading...</td></tr></tbody>
    </table>
  </div>

  <div class="section">
    <h2>Container Logs</h2>
    <div class="logs-container">
      <div class="logs-header">
        <select id="log-select"><option value="">Select a container...</option></select>
      </div>
      <div class="logs" id="logs">Select a container to view logs</div>
    </div>
  </div>

<script>
function fmtUptime(s) {
  var d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  if (d > 0) return d + 'd ' + h + 'h ' + m + 'm';
  if (h > 0) return h + 'h ' + m + 'm';
  return m + 'm';
}
function fmtBytes(b) {
  if (b > 1073741824) return (b / 1073741824).toFixed(2) + ' GB';
  if (b > 1048576) return (b / 1048576).toFixed(1) + ' MB';
  if (b > 1024) return (b / 1024).toFixed(0) + ' KB';
  return b + ' B';
}
function fmtDisk(used, total) {
  if (total >= 1024) return (used/1024).toFixed(1) + ' / ' + (total/1024).toFixed(1) + ' GB';
  return Math.round(used) + ' / ' + Math.round(total) + ' MB';
}
function pct(a, b) { return b > 0 ? Math.round(a / b * 100) : 0; }
function barColor(p) { return p > 90 ? '#ef4444' : p > 70 ? '#eab308' : '#3b82f6'; }

function cardWithBar(label, value, used, total, color) {
  var p = pct(used, total);
  return '<div class="card"><div class="label">' + label + '</div><div class="value ' + color + '">' + value + '</div>' +
    '<div class="sub">' + p + '% used</div>' +
    '<div class="bar"><div class="fill" style="width:' + p + '%;background:' + barColor(p) + '"></div></div></div>';
}
function card(label, value, sub, color) {
  return '<div class="card"><div class="label">' + label + '</div><div class="value ' + color + '">' + value + '</div>' +
    (sub ? '<div class="sub">' + sub + '</div>' : '') + '</div>';
}
function info(k, v) {
  return '<div class="info-item"><span class="k">' + k + '</span><span class="v mono">' + (v || '-') + '</span></div>';
}

async function refresh() {
  try {
    var res = await fetch('/api/status');
    var d = await res.json();
    var m = d.metrics;

    document.getElementById('worker-name').innerHTML = d.worker_name +
      '<span>up ' + fmtUptime(m.uptime_seconds) + '</span>';

    var cpuPct = pct(m.cpu_percent, 100);
    document.getElementById('metrics').innerHTML =
      '<div class="card"><div class="label">CPU</div><div class="value blue">' + m.cpu_percent.toFixed(1) + '%</div>' +
        '<div class="sub">' + m.cpu_cores + ' cores</div>' +
        '<div class="bar"><div class="fill" style="width:' + cpuPct + '%;background:' + barColor(cpuPct) + '"></div></div></div>' +
      cardWithBar('Memory', Math.round(m.memory_used_mb) + ' / ' + Math.round(m.memory_total_mb) + ' MB', m.memory_used_mb, m.memory_total_mb, 'purple') +
      cardWithBar('Disk', fmtDisk(m.disk_used_mb, m.disk_total_mb), m.disk_used_mb, m.disk_total_mb, 'yellow') +
      card('Containers', m.container_running_count + ' / ' + m.container_count, 'running / total', 'green') +
      card('Network', fmtBytes(m.network_rx_bytes) + ' rx', fmtBytes(m.network_tx_bytes) + ' tx', 'gray') +
      (m.swap_total_mb > 0 ? cardWithBar('Swap', Math.round(m.swap_used_mb) + ' / ' + Math.round(m.swap_total_mb) + ' MB', m.swap_used_mb, m.swap_total_mb, 'red') : card('Processes', m.process_count, 'total', 'gray'));

    document.getElementById('info').innerHTML =
      info('Hostname', d.hostname) +
      info('OS / Arch', d.os + ' / ' + d.arch) +
      info('Docker', d.docker_version) +
      info('Go', d.go_version);

    document.getElementById('resources').innerHTML =
      info('Load Average', m.load_avg_1.toFixed(2) + ' / ' + m.load_avg_5.toFixed(2) + ' / ' + m.load_avg_15.toFixed(2)) +
      info('CPU Cores', m.cpu_cores) +
      info('Processes', m.process_count) +
      info('System Uptime', fmtUptime(m.uptime_seconds));
  } catch(e) { console.error('status fetch failed', e); }

  try {
    var res = await fetch('/api/containers');
    var containers = await res.json();
    var tbody = document.getElementById('containers');
    var select = document.getElementById('log-select');
    var prev = select.value;

    if (!containers || containers.length === 0) {
      tbody.innerHTML = '<tr><td colspan="5" class="empty">No containers</td></tr>';
      return;
    }

    tbody.innerHTML = containers.map(function(c) {
      return '<tr>' +
        '<td style="color:white;font-weight:500">' + c.name + '</td>' +
        '<td class="mono" style="color:#888">' + c.image + '</td>' +
        '<td><span class="dot ' + c.state + '"></span>' + c.state + '</td>' +
        '<td style="color:#888">' + c.status + '</td>' +
        '<td class="mono" style="color:#555">' + c.id + '</td></tr>';
    }).join('');

    select.innerHTML = '<option value="">Select a container...</option>' +
      containers.map(function(c) {
        return '<option value="' + c.id + '"' + (c.id === prev ? ' selected' : '') + '>' + c.name + '</option>';
      }).join('');
  } catch(e) { console.error('containers fetch failed', e); }
}

async function loadLogs() {
  var id = document.getElementById('log-select').value;
  var el = document.getElementById('logs');
  if (!id) { el.textContent = 'Select a container to view logs'; return; }
  try {
    var res = await fetch('/api/containers/' + id + '/logs?tail=200');
    var text = await res.text();
    el.textContent = text || '(no logs)';
    el.scrollTop = el.scrollHeight;
  } catch(e) { el.textContent = 'Failed to load logs'; }
}

document.getElementById('log-select').addEventListener('change', loadLogs);
refresh();
setInterval(refresh, 5000);
setInterval(loadLogs, 10000);
</script>
</body>
</html>`
