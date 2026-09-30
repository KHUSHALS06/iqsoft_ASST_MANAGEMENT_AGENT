package main

// The dashboard is one HTML page served at http://localhost:8080
// (Device-supplied text is always shown with textContent, never innerHTML.)
const dashboardHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Endpoint Manager</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
 body{font:14px Segoe UI,system-ui,sans-serif;margin:24px;color:#1a1a1a;background:#f6f7f9}
 h1{font-size:22px;margin:0 0 16px} h2{font-size:17px;margin:0 0 8px} h3{font-size:15px;margin:18px 0 6px}
 table{border-collapse:collapse;width:100%;background:#fff}
 th,td{border:1px solid #dde;padding:6px 10px;text-align:left;vertical-align:top} th{background:#eceff4}
 tr.dev{cursor:pointer} tr.dev:hover{background:#eef4ff} tr.sel{background:#dce9ff}
 .on{color:#0a7d2c;font-weight:600}.off{color:#b00020;font-weight:600}
 button{padding:5px 12px;margin-left:6px;cursor:pointer} input{padding:5px}
 .box{background:#fff;border:1px solid #dde;padding:14px;margin-top:18px;border-radius:6px}
 .kv td:first-child{width:170px;color:#555;background:#fafbfc} #err,#jobmsg{color:#b00020;margin-left:10px}
 #tokbox{margin-top:10px;font-family:Consolas,monospace;background:#fffbe6;padding:8px;display:none;white-space:pre-wrap}
</style></head><body>
<h1>Endpoint Manager</h1>
<div>
 <input id="key" type="password" placeholder="Admin key" size="24">
 <button id="load">Load</button>
 <button id="newtok">New enrollment token</button>
 <span id="err"></span>
</div>
<div id="tokbox"></div>
<div class="box"><h2>Devices</h2>
<table><thead><tr><th>Computer</th><th>Status</th><th>Last seen</th><th>Model</th><th>OS</th><th>User</th><th>Apps</th></tr></thead>
<tbody id="rows"><tr><td colspan="7">Enter the admin key and click Load.</td></tr></tbody></table></div>
<div id="detail"></div>
<script>
var $ = function(id){ return document.getElementById(id); };
function el(tag, text, cls){ var e=document.createElement(tag); if(text!=null) e.textContent=text; if(cls) e.className=cls; return e; }
function row(cells){ var tr=el('tr'); cells.forEach(function(c){ tr.appendChild(typeof c==='string'?el('td',c):c); }); return tr; }
var selected = null, selectedDev = null, invCache = null, doneSeen = null;

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

function loadList(){
  api('/admin/devices').then(function(list){
    $('err').textContent = '';
    sessionStorage.setItem('adminkey', $('key').value);
    var tb=$('rows'); tb.replaceChildren();
    if(!list.length){ tb.appendChild(row(['No devices yet. Click "New enrollment token" and enroll an agent.','','','','','',''])); return; }
    list.sort(function(a,b){ return a.hostname.localeCompare(b.hostname); });
    list.forEach(function(d){
      var tr = row([d.hostname, el('td', d.online?'Online':'Offline', d.online?'on':'off'), ago(d.last_seen), d.model, d.os, d.user, d.apps?String(d.apps):'']);
      tr.className = 'dev' + (d.id===selected?' sel':'');
      tr.onclick = function(){ selected=d.id; selectedDev=d; doneSeen=null; loadDetail(d); loadList(); };
      tb.appendChild(tr);
    });
  }).catch(fail);
}

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

function renderDetail(d, inv){
  var hw = inv.hardware || {}, box = el('div',null,'box');
  box.appendChild(el('h2', d.hostname+'  ('+d.id+')'));
  var kv = el('table',null,'kv');
  var disks = (hw.disks||[]).map(function(x){ return x.model+' ('+x.size_gb+' GB)'; }).join(', ');
  var nics = (hw.nics||[]).map(function(n){ return n.description+'  MAC '+(n.mac||'-')+'  IP '+(n.ips||[]).join(', '); }).join('\n');
  [['Manufacturer / model', (hw.manufacturer||'')+' '+(hw.model||'')], ['Serial number', hw.serial||''],
   ['CPU', (hw.cpu||'')+' ('+hw.cpu_cores+' cores)'], ['RAM', hw.ram_gb+' GB'], ['Disks', disks],
   ['Operating system', (hw.os_name||'')+' '+(hw.os_version||'')], ['BIOS', hw.bios_version||''],
   ['Logged-in user', inv.logged_in_user||''], ['Network', nics]].forEach(function(p){
    var v=el('td', p[1]); v.style.whiteSpace='pre-wrap'; kv.appendChild(row([p[0], v]));
  });
  box.appendChild(kv);
  var sw = (inv.software||[]).slice().sort(function(a,b){ return a.name.toLowerCase()<b.name.toLowerCase()?-1:1; });
  box.appendChild(el('h3', 'Installed software ('+sw.length+')'));
  var f = el('input'); f.placeholder='Filter by name, publisher, type or location'; f.size=30; box.appendChild(f);
  var t = el('table'); t.style.marginTop='8px';
  t.appendChild(row(['Name','Version','Publisher','Type','Installed','Install location']));
  var body = el('tbody'); t.appendChild(body); box.appendChild(t);
  function draw(){
    var q=f.value.toLowerCase(); body.replaceChildren();
    sw.forEach(function(a){
      if(q && (a.name+' '+a.publisher+' '+(a.source||'')+' '+(a.install_location||'')).toLowerCase().indexOf(q)<0) return;
      var ins=(a.install_date||'').replace(/^(\d{4})(\d\d)(\d\d)$/,'$1-$2-$3');
      var loc=el('td', a.install_location||''); loc.style.wordBreak='break-all';
      body.appendChild(row([a.name, a.version, a.publisher, a.source||'', ins, loc]));
    });
  }
  f.oninput = draw; draw();
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

function loadJobs(){
  if(!selected || !$('jobs')) return;
  var dev = selectedDev;
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
$('newtok').onclick = function(){
  api('/admin/token','POST').then(function(r){
    var b=$('tokbox'); b.style.display='block'; $('err').textContent='';
    b.textContent = 'Token (single use): '+r.token+'\n\nOn the computer, run:\n  go run ./agent -enroll '+r.token;
  }).catch(fail);
};
$('key').value = sessionStorage.getItem('adminkey') || '';
if($('key').value) loadList();
setInterval(function(){ if($('key').value && !$('err').textContent){ loadList(); loadJobs(); } }, 5000);
</script></body></html>`