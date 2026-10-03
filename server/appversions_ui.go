package main

import "strings"

// The "App versions" panel. It is a second <script> appended to the existing
// dashboard page, so dashboard.go does not need to change. It reuses the
// helpers defined there ($, el, row, api, ago) and, like the original, only
// ever puts server/device-supplied text into the page with textContent.
//
// It adds two things:
//   - an "App versions" box between the device list and the device detail
//   - an "App versions" section inside the selected device's Actions box
//     (done by wrapping actionsBox, so it reloads whenever the detail does)
const appVersionsPanel = `<script>
(function(){
// No ADMIN_KEY on the server? Then the admin API answers without a key:
// hide the key box and load the dashboard automatically.
fetch('/admin/devices').then(function(r){
  if(!r.ok) return;
  var k = $('key'); k.value = '-'; k.style.display = 'none';
  $('load').click();
}).catch(function(){});
var expanded = null;   // package id whose per-device list is open
var $a = {};           // form inputs

function enc(s){ return encodeURIComponent(s); }
function ruleText(p){
  if(!p.version) return 'any version';
  return (p.policy==='exact' ? '= ' : '>= ') + p.version;
}
var LABEL = {compliant:'Compliant', outdated:'Outdated', missing:'Missing', ahead:'Newer than desired', unknown:'No inventory'};
function statusCell(st){
  return el('td', LABEL[st]||st, st==='compliant' ? 'on' : (st==='outdated'||st==='missing' ? 'off' : null));
}

/* ---------------- winget autocomplete ---------------- */
// One shared dropdown, attached to any input: type "wha" and matching winget
// packages (name, id, latest version, source) appear; click or Enter to pick.
var dd = el('div');
dd.style.cssText = 'position:fixed;z-index:60;background:#fff;border:1px solid #b7bfd0;box-shadow:0 6px 18px rgba(0,0,0,.18);max-height:340px;overflow:auto;display:none;font-size:13px';
document.body.appendChild(dd);
var verList = el('datalist'); verList.id = 'ver-list'; document.body.appendChild(verList);

function loadVersions(id){
  verList.replaceChildren();
  api('/admin/winget/versions?id=' + enc(id)).then(function(vs){
    vs.forEach(function(v){ verList.appendChild(new Option(v)); });
  }).catch(function(){});
}

function attachSuggest(input, onPick){
  var timer = null, seq = 0, items = [], active = -1;
  function hide(){ dd.style.display = 'none'; active = -1; }
  function place(){
    var r = input.getBoundingClientRect();
    dd.style.left = r.left + 'px'; dd.style.top = r.bottom + 'px';
    dd.style.minWidth = Math.max(460, r.width) + 'px'; dd.style.display = 'block';
  }
  function note(t){ var d = el('div', t); d.style.cssText = 'padding:8px 10px;color:#555'; dd.replaceChildren(d); place(); }
  function pick(it){ hide(); onPick(it); }
  function draw(){
    if(!items.length){ note('No packages found'); return; }
    dd.replaceChildren();
    items.forEach(function(it, i){
      var r = el('div');
      r.style.cssText = 'padding:6px 10px;cursor:pointer;border-bottom:1px solid #eef;display:flex;gap:10px;align-items:baseline';
      r.style.background = (i === active) ? '#dce9ff' : '#fff';
      var n = el('span', it.name); n.style.cssText = 'font-weight:600;flex:1';
      var id = el('span', it.id); id.style.cssText = 'font-family:Consolas,monospace;color:#444';
      var v = el('span', it.version || ''); v.style.cssText = 'color:#0a7d2c;min-width:90px;text-align:right';
      var sr = el('span', it.source || ''); sr.style.cssText = 'color:#888;min-width:55px';
      [n, id, v, sr].forEach(function(x){ r.appendChild(x); });
      r.onmousedown = function(e){ e.preventDefault(); pick(it); };
      dd.appendChild(r);
    });
    place();
  }
  function search(){
    var q = input.value.trim(), my = ++seq;
    if(!q){ hide(); return; }
    note('Searching winget...');
    api('/admin/winget/search?q=' + enc(q)).then(function(list){
      if(my !== seq) return;
      items = list; active = -1; draw();
    }).catch(function(e){ if(my === seq) note(e.message); });
  }
  input.addEventListener('input', function(){ clearTimeout(timer); timer = setTimeout(search, 350); });
  input.addEventListener('keydown', function(e){
    if(dd.style.display === 'none' || !items.length) return;
    if(e.key === 'ArrowDown'){ active = (active + 1) % items.length; draw(); e.preventDefault(); }
    else if(e.key === 'ArrowUp'){ active = (active <= 0 ? items.length : active) - 1; draw(); e.preventDefault(); }
    else if(e.key === 'Enter' && active >= 0){ e.preventDefault(); pick(items[active]); }
    else if(e.key === 'Escape'){ hide(); }
  });
  input.addEventListener('blur', function(){ setTimeout(hide, 200); });
}

/* ---------------- fleet panel ---------------- */
var box = el('div', null, 'box');
box.appendChild(el('h2', 'App versions (desired state)'));
var hint = el('p', 'Set the version every device should have. Compliance is calculated from each device\'s last inventory; "Push" queues a winget install/upgrade to that exact version on every device that needs it.');
hint.style.color = '#555'; hint.style.margin = '0 0 10px';
box.appendChild(hint);

var form = el('div');
function inp(key, ph, size){ var i = el('input'); i.placeholder = ph; i.size = size; i.style.marginRight = '6px'; $a[key] = i; form.appendChild(i); return i; }
function sel(key, opts){ var s = el('select'); s.style.padding = '5px'; s.style.marginRight = '6px';
  opts.forEach(function(o){ s.appendChild(new Option(o[1], o[0])); }); $a[key] = s; form.appendChild(s); }
inp('pkg', 'winget id, e.g. Google.Chrome', 28);
inp('name', 'name in Installed software, e.g. Google Chrome', 34);
sel('mt', [['equals','name equals'],['prefix','name starts with'],['contains','name contains']]);
sel('rule', [['minimum','at least version'],['exact','exactly version']]);
inp('ver', 'version (blank = any)', 18);
var saveBtn = el('button', 'Save policy'); saveBtn.style.marginLeft = '0'; form.appendChild(saveBtn);
var msg = el('span'); msg.style.marginLeft = '10px'; form.appendChild(msg);
box.appendChild(form);

var listDiv = el('div'); listDiv.style.marginTop = '12px'; box.appendChild(listDiv);
var devDiv = el('div'); box.appendChild(devDiv);
$('detail').parentNode.insertBefore(box, $('detail'));
attachSuggest($a.pkg, function(it){
  $a.pkg.value = it.id;
  $a.name.value = it.name;
  if(it.version) $a.ver.value = it.version;
  loadVersions(it.id);
});
$a.ver.setAttribute('list', 'ver-list');

function say(t, bad){ msg.textContent = t; msg.style.color = bad ? '#b00020' : '#0a7d2c'; }

saveBtn.onclick = function(){
  api('/admin/app-policies', 'POST', {package_id: $a.pkg.value, name: $a.name.value, match_type: $a.mt.value, policy: $a.rule.value, version: $a.ver.value})
    .then(function(){ say('Saved'); refresh(); })
    .catch(function(e){ say(e.message, true); });
};

function pushResult(r){
  var reasons = {};
  (r.skipped||[]).forEach(function(s){ reasons[s.reason] = (reasons[s.reason]||0) + 1; });
  var parts = Object.keys(reasons).map(function(k){ return reasons[k] + ' ' + k; });
  return 'Queued ' + r.queued + ' job(s)' + (parts.length ? ' - skipped: ' + parts.join('; ') : '');
}

function drawList(list){
  if(!list.length){ listDiv.replaceChildren(el('p', 'No app policies yet. Add one above.')); return; }
  var t = el('table');
  t.appendChild(row(['Package', 'Matches name', 'Desired', 'Compliant', 'Outdated', 'Missing', 'Other', 'Last push', 'Actions']));
  list.forEach(function(p){
    var other = p.ahead + p.unknown;
    var cOK = el('td', p.compliant + ' / ' + p.total, p.total && p.compliant===p.total ? 'on' : null);
    var cOld = el('td', String(p.outdated), p.outdated ? 'off' : null);
    var cMiss = el('td', String(p.missing), p.missing ? 'off' : null);
    var lp = el('td', p.last_push_count ? ago(p.last_push_at) + ' (' + p.last_push_count + ')' : 'never');
    var acts = el('td');
    var need = p.outdated + p.missing;
    var push = el('button', 'Push to all'); push.style.marginLeft = '0';
    push.title = 'Install/upgrade ' + p.package_id + (p.version ? ' to ' + p.version : '') + ' on every device that needs it';
    push.onclick = function(){
      if(!need){ say('Every device with an inventory is already compliant for ' + p.package_id); return; }
      if(!confirm('Queue ' + p.package_id + (p.version ? ' ' + p.version : '') + ' on ' + need + ' device(s)? Offline devices will run it when they next check in.')) return;
      api('/admin/app-policies/' + enc(p.package_id) + '/push', 'POST', {}).then(function(r){ say(pushResult(r)); refresh(); }).catch(function(e){ say(e.message, true); });
    };
    var det = el('button', expanded===p.package_id ? 'Hide devices' : 'Devices');
    det.onclick = function(){ expanded = (expanded===p.package_id) ? null : p.package_id; refresh(); };
    var ed = el('button', 'Edit');
    ed.onclick = function(){ $a.pkg.value = p.package_id; $a.name.value = p.name; $a.mt.value = p.match_type; $a.rule.value = p.policy; $a.ver.value = p.version; };
    var del = el('button', 'Delete');
    del.onclick = function(){
      if(!confirm('Delete the policy for ' + p.package_id + '? Nothing is uninstalled from devices.')) return;
      api('/admin/app-policies/' + enc(p.package_id), 'DELETE').then(function(){ if(expanded===p.package_id) expanded = null; refresh(); }).catch(function(e){ say(e.message, true); });
    };
    [push, det, ed, del].forEach(function(b){ acts.appendChild(b); });
    t.appendChild(row([p.package_id, p.name + ' (' + p.match_type + ')', ruleText(p), cOK, cOld, cMiss, other ? String(other) : '', lp, acts]));
  });
  listDiv.replaceChildren(t);
}

function drawDevices(pkg, list){
  var order = {outdated:0, missing:1, ahead:2, unknown:3, compliant:4};
  list.sort(function(a,b){ return (order[a.status]-order[b.status]) || a.hostname.localeCompare(b.hostname); });
  var t = el('table'); t.style.marginTop = '8px';
  t.appendChild(row(['Computer', 'Device status', 'Installed version', 'Compliance', 'Update queued']));
  list.forEach(function(d){
    t.appendChild(row([d.hostname, el('td', d.online ? 'Online' : 'Offline', d.online ? 'on' : 'off'), d.installed || '', statusCell(d.status), d.queued ? 'yes' : '']));
  });
  var h = el('h3', 'Devices - ' + pkg);
  devDiv.replaceChildren(h, t);
}

function refresh(){
  if(!$('key').value) return;
  api('/admin/app-policies').then(function(list){
    drawList(list);
    var open = expanded;
    if(!open || !list.some(function(p){ return p.package_id===open; })){ expanded = null; devDiv.replaceChildren(); return; }
    return api('/admin/app-policies/' + enc(open) + '/compliance').then(function(d){ if(expanded===open) drawDevices(open, d); });
  }).catch(function(e){ listDiv.replaceChildren(el('p', e.message)); });
}

/* ------------- per-device section in the Actions box ------------- */
var origActions = actionsBox;
actionsBox = function(d){
  var b = origActions(d);
  var pid = b.querySelector('input[placeholder^="winget package id"]');
  var pv = b.querySelector('input[placeholder^="version"]');
  if(pid){ attachSuggest(pid, function(it){ pid.value = it.id; loadVersions(it.id); }); }
  if(pv){ pv.setAttribute('list', 'ver-list'); }
  b.appendChild(el('h3', 'App versions (desired vs installed)'));
  var holder = el('div'); b.appendChild(holder);
  api('/admin/devices/' + d.id + '/compliance').then(function(list){
    if(selected !== d.id) return;
    if(!list.length){ holder.appendChild(el('p', 'No app policies defined.')); return; }
    var t = el('table');
    t.appendChild(row(['Package', 'Desired', 'Installed', 'Compliance', '']));
    list.forEach(function(it){
      var act = el('td');
      if((it.status==='outdated' || it.status==='missing') && !it.queued){
        var pb = el('button', it.status==='missing' ? 'Install' : 'Update'); pb.style.marginLeft = '0';
        pb.onclick = function(){
          api('/admin/app-policies/' + enc(it.package_id) + '/push', 'POST', {device_ids: [d.id]}).then(function(r){
            $('jobmsg').textContent = pushResult(r); loadJobs(); refresh();
          }).catch(function(e){ $('jobmsg').textContent = e.message; });
        };
        act.appendChild(pb);
      } else if(it.queued){ act.textContent = 'queued'; }
      t.appendChild(row([it.package_id, ruleText({version: it.desired, policy: it.policy}), it.installed || '', statusCell(it.status), act]));
    });
    holder.appendChild(t);
  }).catch(function(){});
  return b;
};

$('load').addEventListener('click', refresh);
setInterval(refresh, 10000);
refresh();
})();
</script>`

// dashboardPage is the existing dashboard with the App versions panel appended.
var dashboardPage = strings.Replace(dashboardHTML, "</body></html>", appVersionsPanel+restrictionsPanel+"</body></html>", 1)