package web

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Lattice Runner</title>
<link rel="icon" type="image/svg+xml" href="data:image/svg+xml,%3Csvg viewBox='0 0 512 512' xmlns='http://www.w3.org/2000/svg'%3E%3Crect width='512' height='512' fill='%23000'/%3E%3Cg transform='rotate(45 256 256)'%3E%3Crect x='140' y='140' width='232' height='232' fill='%23d97706'/%3E%3C/g%3E%3Cg transform='rotate(45 256 256)'%3E%3Crect x='242' y='242' width='28' height='28' fill='%23000'/%3E%3C/g%3E%3C/svg%3E">
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0;}
:root{
  --bg:#070707;--surface:#0c0c0c;--surface-2:#101010;
  --border:#1a1a1a;--border-2:#222;
  --text:#a0a0a0;--text-bright:#d8d8d8;--text-dim:#444;
  --amber:#d97706;--green:#16a34a;--red:#b91c1c;
}
body{background:var(--bg);color:var(--text);font-family:'Courier New','Lucida Console',monospace;font-size:12px;line-height:1.4;min-height:100vh;}

/* ── System bar ── */
.sysbar{background:#000;border-bottom:1px solid var(--border-2);padding:0 16px;height:34px;display:flex;align-items:center;gap:18px;position:sticky;top:0;z-index:100;}
.sysbar-brand{display:flex;align-items:center;gap:8px;border-right:1px solid var(--border-2);padding-right:18px;}
.sysbar-brand svg{width:18px;height:18px;display:block;}
.sysbar-brand span{font-size:11px;font-weight:700;letter-spacing:0.18em;text-transform:uppercase;color:var(--text-bright);}
.sysbar-item{display:flex;align-items:center;gap:5px;}
.sbl{font-size:9px;color:var(--text-dim);text-transform:uppercase;letter-spacing:0.1em;}
.sbv{font-size:11px;color:var(--text-bright);}
.sysbar-right{margin-left:auto;display:flex;align-items:center;gap:16px;}

/* ── LED ── */
.led{width:7px;height:7px;border-radius:50%;display:inline-block;flex-shrink:0;}
.led.green{background:#22c55e;box-shadow:0 0 5px #22c55e99;}
.led.amber{background:#d97706;box-shadow:0 0 5px #d9770699;}
.led.red{background:#ef4444;box-shadow:0 0 5px #ef444499;}

/* ── Layout ── */
.main{padding:14px 16px;max-width:1400px;margin:0 auto;}
.gap{margin-top:16px;}

/* ── Section header ── */
.shdr{display:flex;align-items:center;gap:8px;margin-bottom:6px;margin-top:18px;}
.shdr:first-child{margin-top:0;}
.stitle{font-size:9px;font-weight:700;letter-spacing:0.2em;text-transform:uppercase;color:var(--text-dim);white-space:nowrap;}
.sline{flex:1;height:1px;background:var(--border);}

/* ── Metric grid ── */
.mgrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(145px,1fr));gap:1px;background:var(--border);}
.mcell{background:var(--surface);padding:10px 12px;position:relative;}
.mcell::before{content:'';position:absolute;top:0;left:0;right:0;height:2px;background:var(--border-2);}
.mcell.ok::before{background:#16a34a;}
.mcell.warn::before{background:#d97706;}
.mcell.crit::before{background:#b91c1c;}
.mlbl{font-size:9px;letter-spacing:0.14em;text-transform:uppercase;color:var(--text-dim);margin-bottom:5px;}
.mval{font-size:17px;font-weight:700;color:var(--text-bright);line-height:1;margin-bottom:3px;}
.msub{font-size:10px;color:var(--text-dim);}
.mbar{height:2px;background:var(--border-2);margin-top:7px;}
.mbar-fill{height:100%;transition:width 0.4s;}

/* ── Key-value table ── */
.kvt{width:100%;border-collapse:collapse;border:1px solid var(--border);}
.kvt tr{border-bottom:1px solid var(--border);}
.kvt tr:last-child{border-bottom:none;}
.kvt tr:nth-child(even) td{background:var(--surface-2);}
.kvt td{padding:5px 10px;font-size:11px;}
.kvt td:first-child{color:var(--text-dim);text-transform:uppercase;letter-spacing:0.08em;font-size:10px;width:42%;white-space:nowrap;}
.kvt td:last-child{color:var(--text-bright);}

/* ── Data table ── */
.dt{width:100%;border-collapse:collapse;border:1px solid var(--border);}
.dt thead tr{background:var(--surface-2);border-bottom:1px solid var(--border-2);}
.dt th{padding:5px 10px;font-size:9px;letter-spacing:0.15em;text-transform:uppercase;color:var(--text-dim);text-align:left;font-weight:700;white-space:nowrap;}
.dt td{padding:5px 10px;font-size:11px;border-bottom:1px solid var(--border);color:var(--text);}
.dt tbody tr:last-child td{border-bottom:none;}
.dt tbody tr:nth-child(even) td{background:var(--surface-2);}
.dt tbody tr:hover td{background:#141414;}
.empty td{text-align:center;color:var(--text-dim);padding:18px;font-size:10px;letter-spacing:0.1em;}

/* ── State badge ── */
.state{display:inline-flex;align-items:center;gap:5px;font-size:10px;letter-spacing:0.08em;text-transform:uppercase;}
.state .led{width:6px;height:6px;}

/* ── Log viewer ── */
.log-bar{background:var(--surface-2);border:1px solid var(--border);border-bottom:none;padding:5px 10px;display:flex;align-items:center;gap:10px;}
.log-bar label{font-size:9px;text-transform:uppercase;letter-spacing:0.1em;color:var(--text-dim);}
.log-bar select{background:var(--bg);border:1px solid var(--border-2);color:var(--text-bright);padding:3px 6px;font-size:11px;font-family:inherit;cursor:pointer;outline:none;}
.log-bar select:focus{border-color:var(--amber);}
.logview{background:#030303;border:1px solid var(--border);padding:12px;font-size:11px;color:#5a8f5a;white-space:pre-wrap;word-break:break-all;max-height:340px;overflow-y:auto;line-height:1.6;}
.logview::-webkit-scrollbar{width:5px;}
.logview::-webkit-scrollbar-track{background:#0a0a0a;}
.logview::-webkit-scrollbar-thumb{background:var(--border-2);}

.two-col{display:grid;grid-template-columns:1fr 1fr;gap:1px;background:var(--border);}
.two-col>*{background:var(--bg);}
@media(max-width:860px){.two-col{grid-template-columns:1fr;}}
</style>
</head>
<body>

<div class="sysbar">
  <div class="sysbar-brand">
    <svg viewBox="0 0 512 512" xmlns="http://www.w3.org/2000/svg">
      <rect width="512" height="512" fill="#000"/>
      <g transform="rotate(45 256 256)"><rect x="140" y="140" width="232" height="232" fill="#d97706"/></g>
      <g transform="rotate(45 256 256)"><rect x="242" y="242" width="28" height="28" fill="#000"/></g>
    </svg>
    <span>Lattice Runner</span>
  </div>
  <div class="sysbar-item">
    <span class="led green" id="sys-led"></span>
    <span class="sbl">Status</span>
    <span class="sbv" id="sys-status">ONLINE</span>
  </div>
  <div class="sysbar-item"><span class="sbl">Node</span><span class="sbv" id="sys-node">—</span></div>
  <div class="sysbar-item"><span class="sbl">Uptime</span><span class="sbv" id="sys-uptime">—</span></div>
  <div class="sysbar-right">
    <div class="sysbar-item" id="lattice-link-wrap"></div>
    <div class="sysbar-item"><span class="sbl">Version</span><span class="sbv" id="sys-version">—</span></div>
    <div class="sysbar-item"><span class="sbv" style="color:var(--text-dim)" id="sys-clock">—</span></div>
  </div>
</div>

<div class="main">
  <div class="shdr"><span class="stitle">System Health</span><div class="sline"></div></div>
  <div class="mgrid" id="metrics"></div>

  <div class="shdr gap"><span class="stitle">System Information</span><div class="sline"></div></div>
  <div class="two-col">
    <table class="kvt" id="info"></table>
    <table class="kvt" id="resources"></table>
  </div>

  <div class="shdr gap"><span class="stitle">Container Inventory</span><div class="sline"></div></div>
  <table class="dt">
    <thead><tr><th>Name</th><th>Image</th><th>State</th><th>Status</th><th>ID</th></tr></thead>
    <tbody id="containers"><tr class="empty"><td colspan="5">LOADING...</td></tr></tbody>
  </table>

  <div class="shdr gap"><span class="stitle">Event Log</span><div class="sline"></div></div>
  <div class="log-bar">
    <label>Container</label>
    <select id="log-select"><option value="">— SELECT —</option></select>
  </div>
  <div class="logview" id="logs">// Select a container to view output</div>
</div>

<script>
!function(){var u='{{LATTICE_URL}}';if(u){var a=document.createElement('a');a.href=u;a.target='_blank';a.rel='noopener noreferrer';a.textContent='OPEN LATTICE \u2197';a.style.cssText='font-size:10px;color:#d97706;text-decoration:none;letter-spacing:0.1em;';a.onmouseover=function(){a.style.textDecoration='underline';};a.onmouseout=function(){a.style.textDecoration='none';};document.getElementById('lattice-link-wrap').appendChild(a);}}();

function tick(){document.getElementById('sys-clock').textContent=new Date().toISOString().replace('T',' ').slice(0,19)+' UTC';}
tick();setInterval(tick,1000);

function fmtUp(s){var d=Math.floor(s/86400),h=Math.floor((s%86400)/3600),m=Math.floor((s%3600)/60);return d>0?d+'d '+h+'h '+m+'m':h>0?h+'h '+m+'m':m+'m '+(s%60)+'s';}
function fmtMem(mb){return mb>=1024?(mb/1024).toFixed(1)+' GiB':Math.round(mb)+' MiB';}
function fmtNet(b){return b>1073741824?(b/1073741824).toFixed(2)+' GB':b>1048576?(b/1048576).toFixed(1)+' MB':b>1024?(b/1024).toFixed(0)+' KB':b+' B';}
function pct(a,b){return b>0?Math.min(100,Math.round(a/b*100)):0;}
function sev(p){return p>90?'crit':p>70?'warn':'ok';}
function bclr(p){return p>90?'#ef4444':p>70?'#f59e0b':'#22c55e';}
function scol(s){return s==='running'?'green':s==='exited'?'red':'amber';}

function mcell(lbl,val,sub,p,cls){
  var bar=p!=null?'<div class="mbar"><div class="mbar-fill" style="width:'+p+'%;background:'+bclr(p)+'"></div></div>':'';
  return '<div class="mcell '+cls+'"><div class="mlbl">'+lbl+'</div><div class="mval">'+val+'</div><div class="msub">'+sub+'</div>'+bar+'</div>';
}
function kv(k,v){return '<tr><td>'+k+'</td><td>'+v+'</td></tr>';}

async function refresh(){
  try{
    var r=await fetch('/api/status'),d=await r.json(),m=d.metrics;
    document.getElementById('sys-node').textContent=d.worker_name||d.hostname||'—';
    document.getElementById('sys-uptime').textContent=fmtUp(m.uptime_seconds);
    document.getElementById('sys-version').textContent=d.version||'—';
    document.getElementById('sys-led').className='led green';
    document.getElementById('sys-status').textContent='ONLINE';

    var cp=pct(m.cpu_percent,100),mp=pct(m.memory_used_mb,m.memory_total_mb),dp=pct(m.disk_used_mb,m.disk_total_mb);
    document.getElementById('metrics').innerHTML=
      mcell('CPU',m.cpu_percent.toFixed(1)+'%',m.cpu_cores+' cores',cp,sev(cp))+
      mcell('Memory',fmtMem(m.memory_used_mb),fmtMem(m.memory_total_mb)+' total',mp,sev(mp))+
      mcell('Disk',fmtMem(m.disk_used_mb),fmtMem(m.disk_total_mb)+' total',dp,sev(dp))+
      mcell('Containers',m.container_running_count+' running',m.container_count+' total',null,'ok')+
      mcell('Net RX / TX',fmtNet(m.network_rx_bytes),fmtNet(m.network_tx_bytes),null,'ok')+
      (m.swap_total_mb>0?mcell('Swap',fmtMem(m.swap_used_mb),fmtMem(m.swap_total_mb)+' total',pct(m.swap_used_mb,m.swap_total_mb),sev(pct(m.swap_used_mb,m.swap_total_mb))):mcell('Processes',m.process_count,'total',null,'ok'));

    document.getElementById('info').innerHTML=
      kv('Hostname',d.hostname||'—')+kv('OS',d.os||'—')+kv('Architecture',d.arch||'—')+
      kv('Docker',d.docker_version||'—')+kv('Runtime',d.go_version||'—')+kv('Runner',d.version||'—');

    document.getElementById('resources').innerHTML=
      kv('Load Avg 1m',m.load_avg_1.toFixed(3))+kv('Load Avg 5m',m.load_avg_5.toFixed(3))+
      kv('Load Avg 15m',m.load_avg_15.toFixed(3))+kv('CPU Cores',m.cpu_cores)+
      kv('Processes',m.process_count)+kv('System Uptime',fmtUp(m.uptime_seconds));
  }catch(e){
    document.getElementById('sys-led').className='led red';
    document.getElementById('sys-status').textContent='UNREACHABLE';
  }

  try{
    var r=await fetch('/api/containers'),cs=await r.json();
    var tb=document.getElementById('containers'),sel=document.getElementById('log-select'),prev=sel.value;
    if(!cs||!cs.length){tb.innerHTML='<tr class="empty"><td colspan="5">NO CONTAINERS FOUND</td></tr>';return;}
    tb.innerHTML=cs.map(function(c){
      return '<tr><td style="color:#d8d8d8;font-weight:700">'+c.name+'</td>'+
        '<td style="color:#444">'+c.image+'</td>'+
        '<td><span class="state"><span class="led '+scol(c.state)+'"></span>'+c.state.toUpperCase()+'</span></td>'+
        '<td style="color:#444">'+c.status+'</td>'+
        '<td style="color:#333;font-size:10px">'+c.id+'</td></tr>';
    }).join('');
    sel.innerHTML='<option value="">— SELECT —</option>'+cs.map(function(c){
      return '<option value="'+c.id+'"'+(c.id===prev?' selected':'')+'>'+c.name+'</option>';
    }).join('');
  }catch(e){}
}

async function loadLogs(){
  var id=document.getElementById('log-select').value,el=document.getElementById('logs');
  if(!id){el.textContent='// Select a container to view output';return;}
  try{
    var r=await fetch('/api/containers/'+id+'/logs?tail=200'),t=await r.text();
    el.textContent=t||'// (no output)';el.scrollTop=el.scrollHeight;
  }catch(e){el.textContent='// Failed to retrieve logs';}
}

document.getElementById('log-select').addEventListener('change',loadLogs);
refresh();setInterval(refresh,5000);setInterval(loadLogs,10000);
</script>
</body>
</html>`

