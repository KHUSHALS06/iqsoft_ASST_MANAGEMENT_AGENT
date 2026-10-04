package main

// The dashboard is one HTML page served at http://localhost:8080
// (Device-supplied text is always shown with textContent, never innerHTML.)
const dashboardHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Endpoint Manager</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
 :root{--bg:#f4f6fa;--card:#fff;--line:#e3e7ee;--txt:#1b2230;--mut:#6b7585;--pri:#2563eb;--ok:#15803d;--bad:#b91c1c}
 *{box-sizing:border-box}
 body{font:14px/1.45 Segoe UI,system-ui,sans-serif;margin:0;color:var(--txt);background:var(--bg)}
 header{position:sticky;top:0;z-index:5;display:flex;gap:10px;align-items:center;padding:10px 20px;background:#0f172a;color:#fff}
 header b{font-size:16px;margin-right:14px} .sp{flex:1}
 header input{width:180px}
 nav{display:flex;gap:4px} nav button{background:transparent;color:#cbd5e1;border:0;padding:7px 14px;border-radius:6px;cursor:pointer;margin:0}
 nav button.act{background:#1e293b;color:#fff}
 .pane{padding:20px;max-width:1500px;margin:0 auto} .pane[hidden]{display:none}
 #app{display:grid;grid-template-columns:300px 1fr;gap:20px;align-items:start}
 aside{position:sticky;top:64px;max-height:calc(100vh - 84px);overflow:auto}
 #q{width:100%;margin-bottom:10px}
 .dc{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:10px 12px;margin-bottom:8px;cursor:pointer}
 .dc:hover{border-color:var(--pri)} .dc.sel{border-color:var(--pri);box-shadow:0 0 0 2px #dbe7ff}
 .dc b{display:block} .dc small{color:var(--mut)}
 .dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px;background:#9aa3b2} .dot.on{background:#22c55e}
 input,select{padding:7px 10px;border:1px solid var(--line);border-radius:6px;font:inherit;background:#fff;color:var(--txt)}
 button{padding:7px 14px;margin-left:6px;border:1px solid var(--line);border-radius:6px;background:#fff;cursor:pointer;font:inherit}
 button:hover{border-color:var(--pri);color:var(--pri)}
 button.primary{background:var(--pri);border-color:var(--pri);color:#fff} button.primary:hover{color:#fff;opacity:.9}
 header button{background:var(--pri);border-color:var(--pri);color:#fff}
 h2{font-size:16px;margin:0 0 10px} h3{font-size:14px;margin:20px 0 8px;color:var(--mut);text-transform:uppercase;letter-spacing:.04em}
 .box{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px;margin-bottom:16px}
 table{border-collapse:collapse;width:100%;background:#fff;border-radius:8px;overflow:hidden}
 th,td{border-bottom:1px solid var(--line);padding:8px 10px;text-align:left;vertical-align:top}
 th{background:#f1f4f9;font-weight:600;color:var(--mut);font-size:12px;text-transform:uppercase;letter-spacing:.03em}
 .kv td:first-child{width:170px;color:var(--mut);background:#fafbfd}
 .on{color:var(--ok);font-weight:600}.off{color:var(--bad);font-weight:600}
 #err,#jobmsg{color:#fca5a5;margin-left:10px} #jobmsg{color:var(--bad)}
 .hint,.empty{color:var(--mut);padding:30px;text-align:center;background:var(--card);border:1px dashed var(--line);border-radius:10px}
 @media(max-width:900px){#app{grid-template-columns:1fr} aside{position:static;max-height:none}}
</style></head><body>
<header><b>Endpoint Manager</b><nav id="tabs"></nav><span class="sp"></span>
 <input id="key" type="password" placeholder="Admin key"><button id="load">Connect</button><span id="err"></span></header>
<div class="pane" id="pane-devices"><div id="app">
 <aside><input id="q" placeholder="Search computers"><div id="list"><p class="hint">Enter the admin key and click Connect.</p></div></aside>
 <section><div id="detail"><p class="hint">Select a computer from the list.</p></div></section>
</div></div>
<div class="pane" id="pane-apps" hidden></div><div class="pane" id="pane-restrict" hidden></div>
<script>
var $ = function(id){ return document.getElementById(id); };
function el(tag, text, cls){ var e=document.createElement(tag); if(text!=null) e.textContent=text; if(cls) e.className=cls; return e; }
function row(cells){ var tr=el('tr'); cells.forEach(function(c){ tr.appendChild(typeof c==='string'?el('td',c):c); }); return tr; }
var selected = null, selectedDev = null, invCache = null, doneSeen = null;
var usageDays = 7;

function api(path, method, body){
  var h = {'X-Admin-Key': $('key').value};
  if(body) h['Content-Type'] = 'application/json';
  return fetch(path, {method: method||'GET', headers: h, body: body?JSON.stringify(body):undefined}).then(function(r){
    if(r.status===401) throw new Error('Wrong admin key');
    if(!r.ok) return r.text().then(function(t){ throw new Error(t.trim()||('Error '+r.status)); });
    return r.json();
  });
}
function ago(t){
  var s=(Date.now()-new Date(t))/1000;
  if(!isFinite(s)||s>3e8) return 'never';
  if(s<90) return Math.round(s)+' sec ago';
  if(s<5400) return Math.round(s/60)+' min ago';
  if(s<172800) return Math.round(s/3600)+' hours ago';
  return Math.round(s/86400)+' days ago';
}
function fail(e){ $('err').textContent = e.message; }

var devList = [];
function drawList(){
  var box=$('list'), q=$('q').value.toLowerCase(); box.replaceChildren();
  var shown = devList.filter(function(d){ return !q || (d.hostname+' '+d.user+' '+d.model).toLowerCase().indexOf(q)>=0; });
  if(!devList.length){ box.appendChild(el('p','No devices yet. Start the agent on a computer and it will appear here.','hint')); return; }
  shown.forEach(function(d){
    var c = el('div', null, 'dc' + (d.id===selected?' sel':''));
    var t = el('b'); t.appendChild(el('span', null, 'dot'+(d.online?' on':''))); t.appendChild(document.createTextNode(d.hostname)); c.appendChild(t);
    c.appendChild(el('small', (d.online?'Online':'Offline ' + ago(d.last_seen))+(d.user?' \u00b7 '+d.user:'')));
    c.onclick = function(){ selected=d.id; selectedDev=d; doneSeen=null; loadDetail(d); drawList(); };
    box.appendChild(c);
  });
}
function loadList(){
  api('/admin/devices').then(function(list){
    $('err').textContent = '';
    sessionStorage.setItem('adminkey', $('key').value);
    list.sort(function(a,b){ return a.hostname.localeCompare(b.hostname); });
    devList = list; drawList();
  }).catch(fail);
}
$('q').oninput = drawList;

function loadDetail(d){
  api('/admin/devices/'+d.id+'/inventory').then(function(inv){
    invCache = inv; renderDetail(d, inv);
  }).catch(function(e){
    var box=el('div',null,'box'); box.appendChild(el('h2', d.hostname));
    box.appendChild(el('p', 'No inventory yet ('+e.message+'). The agent sends it right after it starts.'));
    $('detail').replaceChildren(actionsBox(d), box);
    loadJobs();
  });
}

function tierClass(t){ return (t==='Premium'||t==='Likely premium') ? 'on' : (t==='Unlicensed' ? 'off' : null); }
function licensesSection(inv){
  var wrap = el('div'), lics = inv.licenses || [];
  wrap.appendChild(el('h3', 'Licences & subscriptions ('+lics.length+')'));
  if(!lics.length){ wrap.appendChild(el('p', 'No Microsoft Office or Adobe products found yet. Click Refresh inventory if the agent was just updated.')); return wrap; }
  var t = el('table');
  t.appendChild(row(['Vendor','Product','Version','Type','Tier','Status','How we know']));
  lics.forEach(function(l){
    t.appendChild(row([l.vendor||'', l.product||'', l.version||'', l.type||'', el('td', l.tier||'', tierClass(l.tier)), l.status||'', l.basis||'']));
  });
  wrap.appendChild(t);
  var n = el('small', 'Read from this PC only. Who pays for a subscription, and the Adobe account, are not visible on the device. "Likely premium" means the licence state could not be verified.');
  n.style.color = 'var(--mut)'; wrap.appendChild(n);
  return wrap;
}
function accountsSection(inv){
  var wrap = el('div'), acc = inv.accounts || [];
  wrap.appendChild(el('h3', 'Signed-in accounts ('+acc.length+')'));
  if(!acc.length){ wrap.appendChild(el('p', 'No signed-in app accounts found yet.')); return wrap; }
  var t = el('table');
  t.appendChild(row(['App','Account','Type','Plan','Windows user','Last seen']));
  acc.forEach(function(a){
    var seen = a.seen_at ? ago(a.seen_at) : '';
    if(a.stale) seen += ' (user not signed in now)';
    t.appendChild(row([a.app||'', a.account||'\u2014', [a.kind||'', a.name||''].filter(Boolean).join(' \u00b7 '), a.plan||'', a.user||'', seen]));
  });
  wrap.appendChild(t);
  return wrap;
}

function renderDetail(d, inv){
  var hw = inv.hardware || {}, box = el('div',null,'box');
  box.appendChild(el('h2', d.hostname+'  ('+d.id+')'));
  var kv = el('table',null,'kv');
  var disks = (hw.disks||[]).map(function(x){ return x.model+' ('+x.size_gb+' GB)'; }).join(', ');
  var vols = (hw.volumes||[]).map(function(v){ return v.drive+'  '+(v.label||'')+'  '+v.size_gb+' GB ('+v.free_gb+' GB free)  '+v.type; }).join('\n');
  var nics = (hw.nics||[]).map(function(n){ return n.description+'  MAC '+(n.mac||'-')+'  IP '+(n.ips||[]).join(', '); }).join('\n');
  [['Manufacturer / model', (hw.manufacturer||'')+' '+(hw.model||'')], ['Serial number', hw.serial||''],
   ['CPU', (hw.cpu||'')+' ('+hw.cpu_cores+' cores)'], ['RAM', hw.ram_gb+' GB'], ['Disks', disks], ['Drives', vols],
   ['Operating system', (hw.os_name||'')+' '+(hw.os_version||'')], ['BIOS', hw.bios_version||''],
   ['Logged-in user', inv.logged_in_user||''], ['Network', nics]].forEach(function(p){
    var v=el('td', p[1]); v.style.whiteSpace='pre-wrap'; kv.appendChild(row([p[0], v]));
  });
  box.appendChild(kv);
  box.appendChild(licensesSection(inv));
  box.appendChild(accountsSection(inv));
  var sw = (inv.software||[]).slice().sort(function(a,b){ return a.name.toLowerCase()<b.name.toLowerCase()?-1:1; });
  box.appendChild(el('h3', 'Installed software ('+sw.length+')'));
  var f = el('input'); f.placeholder='Filter by name, publisher, type or location'; f.size=30; box.appendChild(f);
  var ds = el('select'); ds.style.marginLeft='8px'; ds.style.padding='5px';
  var dset = {}; sw.forEach(function(a){ dset[a.drive||'Unknown']=1; });
  ds.appendChild(new Option('All drives',''));
  Object.keys(dset).sort().forEach(function(k){ ds.appendChild(new Option(k,k)); });
  box.appendChild(ds);
  var t = el('table'); t.style.marginTop='8px';
  t.appendChild(row(['Name','Version','Publisher','Type','Drive','Installed','Install location']));
  var body = el('tbody'); t.appendChild(body); box.appendChild(t);
  function draw(){
    var q=f.value.toLowerCase(), dv=ds.value; body.replaceChildren();
    sw.forEach(function(a){
      if(dv && (a.drive||'Unknown')!==dv) return;
      if(q && (a.name+' '+a.publisher+' '+(a.source||'')+' '+(a.drive||'')+' '+(a.install_location||'')).toLowerCase().indexOf(q)<0) return;
      var ins=(a.install_date||'').replace(/^(\d{4})(\d\d)(\d\d)$/,'$1-$2-$3');
      var loc=el('td', a.install_location||''); loc.style.wordBreak='break-all';
      body.appendChild(row([a.name, a.version, a.publisher, a.source||'', a.drive||'', ins, loc]));
    });
  }
  f.oninput = draw; ds.onchange = draw; draw();
  $('detail').replaceChildren(actionsBox(d), box);
  loadJobs();
}

function sendJob(type, params){
  if(!selected) return;
  api('/admin/devices/'+selected+'/jobs', 'POST', {type: type, params: params}).then(function(){
    $('jobmsg').textContent = '';
    loadJobs();
  }).catch(function(e){ $('jobmsg').textContent = e.message; });
}

function actionsBox(d){
  var box = el('div', null, 'box');
  box.appendChild(el('h2', 'Actions'));
  var rb = el('button', 'Refresh inventory'); rb.style.marginLeft = '0';
  rb.onclick = function(){ sendJob('refresh_inventory', {}); };
  box.appendChild(rb);
  var remoteBtn = el('button', 'Remote session', 'primary');
  remoteBtn.title = 'Opens a live screen-share to this device. A red banner is shown on the device the whole time.';
  remoteBtn.onclick = function(){
    if(!d.online){ $('jobmsg').textContent = 'Device is offline'; return; }
    if(!confirm('Start a remote session on '+d.hostname+'? A visible banner will show on that screen for the whole session.')) return;
    var w = window.open('', '_blank');
    api('/admin/devices/'+d.id+'/remote', 'POST').then(function(r){
      $('jobmsg').textContent = '';
      if(w) w.location = r.viewer_url; else $('jobmsg').textContent = 'Popup blocked. Open: ' + r.viewer_url;
    }).catch(function(e){ if(w) w.close(); $('jobmsg').textContent = e.message; });
  };
  box.appendChild(remoteBtn);
  var line = el('div'); line.style.marginTop = '10px';
  var pid = el('input'); pid.placeholder = 'winget package id, e.g. Google.Chrome'; pid.size = 36;
  var pv = el('input'); pv.placeholder = 'version (optional)'; pv.size = 16; pv.style.marginLeft = '6px';
  line.appendChild(pid); line.appendChild(pv);
  [['Install','winget_install'],['Upgrade','winget_upgrade'],['Uninstall','winget_uninstall']].forEach(function(a){
    var b = el('button', a[0]);
    b.onclick = function(){
      var id = pid.value.trim();
      if(!id){ $('jobmsg').textContent = 'Enter a winget package id'; return; }
      if(a[1]==='winget_uninstall' && !confirm('Uninstall '+id+' from '+d.hostname+'?')) return;
      var p = {id: id};
      if(pv.value.trim()) p.version = pv.value.trim();
      sendJob(a[1], p);
    };
    line.appendChild(b);
  });
  var msg = el('span'); msg.id = 'jobmsg'; line.appendChild(msg);
  box.appendChild(line);
  box.appendChild(el('h3', 'Recent jobs'));
  var jl = el('div'); jl.id = 'jobs'; box.appendChild(jl);
  box.appendChild(el('h3', 'Install / uninstall history'));
  var hl = el('div'); hl.id = 'hist'; box.appendChild(hl);
  box.appendChild(el('h3', 'App usage'));
  var us = el('select'); us.style.padding = '5px';
  [['1','Today'],['7','Last 7 days'],['30','Last 30 days']].forEach(function(o){ us.appendChild(new Option(o[1], o[0])); });
  us.value = String(usageDays);
  us.onchange = function(){ usageDays = parseInt(us.value, 10); loadUsage(); };
  box.appendChild(us);
  var ul = el('div'); ul.id = 'usage'; box.appendChild(ul);
  return box;
}

function drawJobs(list){
  var box = $('jobs'); if(!box) return;
  if(!list.length){ box.replaceChildren(el('p', 'No jobs yet.')); return; }
  var t = el('table');
  t.appendChild(row(['Created', 'Action', 'Package', 'Status', 'Output']));
  list.forEach(function(j){
    var p = j.params || {};
    var out = el('td', (j.output||'').slice(-300)); out.style.whiteSpace = 'pre-wrap'; out.title = j.output || '';
    var st = el('td', j.status, j.status==='done' ? 'on' : (j.status==='failed' ? 'off' : null));
    t.appendChild(row([new Date(j.created_at).toLocaleString(), j.type, (p.id||'')+(p.version?' '+p.version:''), st, out]));
  });
  box.replaceChildren(t);
}

function drawHistory(list){
  var box = $('hist'); if(!box) return;
  if(!list.length){ box.replaceChildren(el('p', 'No changes recorded yet. Changes appear after the device sends a second inventory.')); return; }
  var t = el('table');
  t.appendChild(row(['When', 'Change', 'Software', 'Type', 'Version']));
  list.forEach(function(h){
    var kind = h.kind==='installed' ? 'Installed' : (h.kind==='removed' ? 'Removed' : 'Version changed');
    var ver = h.kind==='installed' ? (h.new_version||'') : (h.kind==='removed' ? (h.old_version||'') : (h.old_version||'?')+' \u2192 '+(h.new_version||'?'));
    var k = el('td', kind, h.kind==='installed' ? 'on' : (h.kind==='removed' ? 'off' : null));
    t.appendChild(row([new Date(h.time).toLocaleString(), k, h.name, h.source||'', ver]));
  });
  box.replaceChildren(t);
}

function loadHistory(){
  if(!selected || !$('hist')) return;
  var dev = selectedDev;
  api('/admin/devices/'+selected+'/history').then(function(list){
    if(dev !== selectedDev) return;
    drawHistory(list);
  }).catch(function(){});
}

function fmtDur(sec){
  var m = Math.round(sec/60);
  if(m < 1) return '<1m';
  if(m < 60) return m+'m';
  return Math.floor(m/60)+'h '+(m%60)+'m';
}

function drawUsage(u){
  var box = $('usage'); if(!box) return;
  var days = u.days || [], sessions = u.sessions || [];
  if(!days.length && !sessions.length){ box.replaceChildren(el('p', 'No usage recorded yet. The agent uploads usage every few minutes.')); return; }
  var tot = {}, all = 0;
  days.forEach(function(d){ d.apps.forEach(function(a){ tot[a.app] = (tot[a.app]||0) + a.seconds; all += a.seconds; }); });
  var apps = Object.keys(tot).sort(function(a,b){ return tot[b]-tot[a]; });
  var wrap = el('div');
  var t = el('table'); t.style.marginTop = '8px';
  t.appendChild(row(['App', 'Time used', 'Share']));
  apps.slice(0, 25).forEach(function(a){
    t.appendChild(row([a, fmtDur(tot[a]), all ? Math.round(tot[a]*100/all)+'%' : '']));
  });
  wrap.appendChild(el('p', 'Total active time: '+fmtDur(all)));
  wrap.appendChild(t);
  wrap.appendChild(el('h3', 'Recent sessions'));
  var st = el('table');
  st.appendChild(row(['Started', 'App', 'Duration', 'Ended']));
  sessions.slice(0, 50).forEach(function(x){
    st.appendChild(row([new Date(x.start).toLocaleString(), x.app, fmtDur((new Date(x.end)-new Date(x.start))/1000), new Date(x.end).toLocaleTimeString()]));
  });
  wrap.appendChild(st);
  box.replaceChildren(wrap);
}

function loadUsage(){
  if(!selected || !$('usage')) return;
  var dev = selectedDev;
  api('/admin/devices/'+selected+'/usage?days='+usageDays).then(function(u){
    if(dev !== selectedDev) return;
    drawUsage(u);
  }).catch(function(){});
}

function loadJobs(){
  if(!selected || !$('jobs')) return;
  var dev = selectedDev;
  loadHistory();
  loadUsage();
  api('/admin/devices/'+selected+'/jobs').then(function(list){
    if(dev !== selectedDev) return;
    var ids = {}, newDone = false;
    list.forEach(function(j){
      if(j.status==='done'){ ids[j.id] = 1; if(doneSeen && !doneSeen[j.id]) newDone = true; }
    });
    doneSeen = ids;
    drawJobs(list);
    if(newDone) loadDetail(dev);
  }).catch(function(){});
}

$('load').onclick = loadList;
$('key').value = sessionStorage.getItem('adminkey') || '';
if($('key').value) loadList();
setInterval(function(){ if($('key').value && !$('err').textContent){ loadList(); loadJobs(); } }, 5000);
</script></body></html>`