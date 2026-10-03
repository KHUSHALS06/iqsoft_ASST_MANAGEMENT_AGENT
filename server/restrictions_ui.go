package main

// The "Install / uninstall protection" panel. Like the App versions panel it is
// one more <script> appended to the dashboard page (see the last line of
// appversions_ui.go), so dashboard.go does not change. It reuses the helpers
// defined there ($, el, row, api, ago) and only ever puts server-supplied text
// into the page with textContent.
//
// It adds two things:
//   - a fleet box between the device list and the device detail: the default
//     policy for all PCs, and one status row per PC
//   - a section inside the selected device's Actions box (by wrapping
//     actionsBox again) to give that one PC its own policy
const restrictionsPanel = `<script>
(function(){
var FLAGS = [
  ["block_msi", "Block MSI and per-user installs"],
  ["block_uninstall", "Hide the Uninstall button on installed apps"],
  ["block_user_exe", "Block programs run from Downloads, Desktop and Temp"]
];
var SHORT = {block_msi: "Installs", block_uninstall: "Uninstall hidden", block_user_exe: "Downloads/Desktop/Temp"};
var rows = [], formInit = false;

function isEmpty(p){ return !p.block_msi && !p.block_uninstall && !p.block_user_exe; }
function summary(p){
  var on = FLAGS.filter(function(f){ return p[f[0]]; }).map(function(f){ return SHORT[f[0]]; });
  return on.length ? on.join(", ") : "No restrictions";
}
function same(a, b){ return !!a.block_msi===!!b.block_msi && !!a.block_uninstall===!!b.block_uninstall && !!a.block_user_exe===!!b.block_user_exe; }

function makeChecks(parent){
  var boxes = {};
  FLAGS.forEach(function(f){
    var lab = el("label"); lab.style.cssText = "display:block;margin:4px 0";
    var cb = el("input"); cb.type = "checkbox";
    lab.appendChild(cb); lab.appendChild(document.createTextNode(" " + f[1]));
    parent.appendChild(lab); boxes[f[0]] = cb;
  });
  return boxes;
}
function setChecks(boxes, p){ FLAGS.forEach(function(f){ boxes[f[0]].checked = !!(p && p[f[0]]); }); }
function readChecks(boxes){ var o = {}; FLAGS.forEach(function(f){ o[f[0]] = boxes[f[0]].checked; }); return o; }

// "Applied 5 min ago" / "Failed ..." / "Sending..." for one device row.
function statusCell(r){
  var a = r.applied;
  if(!a) return el("td", r.source === "none" ? "Not managed" : (r.online ? "Sending soon..." : "Waiting for PC to come online"));
  var txt, cls = null;
  if(a.status === "queued"){ txt = "Sending..."; }
  else if(a.status === "failed"){ txt = "Failed " + ago(a.updated_at); cls = "off"; }
  else {
    txt = (isEmpty(a.policy) ? "Cleared " : "Applied ") + ago(a.applied_at); cls = "on";
    if(!same(a.policy, r.policy)) { txt = "Change pending"; cls = null; }
  }
  var td = el("td", txt, cls);
  if(a.detail) td.title = a.detail;
  return td;
}

/* ---------------- fleet panel ---------------- */
var box = el("div", null, "box");
box.appendChild(el("h2", "Install / uninstall protection"));
var hint = el("p", "Choose what standard (non-admin) users may not do on these PCs. Local administrators are not restricted by these settings, and the agent can still install and uninstall apps for you from this dashboard. Try a setting on one PC first (select the PC and use its own section), then set it as the default for all PCs.");
hint.style.color = "#555"; hint.style.margin = "0 0 10px";
box.appendChild(hint);

var form = el("div");
var defBoxes = makeChecks(form);
var defLine = el("p", ""); defLine.style.margin = "8px 0"; defLine.style.color = "#555";
form.appendChild(defLine);
var setBtn = el("button", "Set as default for all PCs"); setBtn.style.marginLeft = "0";
var clearBtn = el("button", "Remove protection from all PCs");
var msg = el("span"); msg.style.marginLeft = "10px";
form.appendChild(setBtn); form.appendChild(clearBtn); form.appendChild(msg);
box.appendChild(form);

var tableDiv = el("div"); tableDiv.style.marginTop = "12px";
box.appendChild(tableDiv);
$("detail").parentNode.insertBefore(box, $("detail"));

function say(t, bad){ msg.textContent = t; msg.style.color = bad ? "#b00020" : "#0a7d2c"; }
function noOwn(){ return rows.filter(function(x){ return x.source !== "device"; }).length; }

setBtn.onclick = function(){
  var p = readChecks(defBoxes);
  if(isEmpty(p)){ say("Tick at least one protection, or use Remove protection from all PCs.", true); return; }
  if(!confirm("Apply " + summary(p) + " to " + noOwn() + " PC(s) that have no policy of their own? Online PCs get it within a minute; offline PCs when they reconnect.")) return;
  api("/admin/restrictions/default", "POST", p).then(function(){ say("Default saved"); refresh(); }).catch(function(e){ say(e.message, true); });
};
clearBtn.onclick = function(){
  if(!confirm("Remove all protection from the " + noOwn() + " PC(s) that follow the default? PCs with their own policy are not changed.")) return;
  var p = {block_msi: false, block_uninstall: false, block_user_exe: false};
  api("/admin/restrictions/default", "POST", p).then(function(){ setChecks(defBoxes, p); say("Protection is being removed"); refresh(); }).catch(function(e){ say(e.message, true); });
};

function drawTable(){
  if(!rows.length){ tableDiv.replaceChildren(el("p", "No devices yet.")); return; }
  var t = el("table");
  t.appendChild(row(["Computer", "Device", "Policy", "Protection status"]));
  rows.forEach(function(r){
    var pol = r.source === "none" ? "Not managed" : summary(r.policy) + " (" + (r.source === "device" ? "own policy" : "default") + ")";
    t.appendChild(row([r.hostname, el("td", r.online ? "Online" : "Offline", r.online ? "on" : "off"), pol, statusCell(r)]));
  });
  tableDiv.replaceChildren(t);
}

function refresh(){
  if(!$("key").value) return;
  api("/admin/restrictions").then(function(r){
    rows = r.devices || [];
    if(!formInit){ setChecks(defBoxes, r["default"]); formInit = true; }
    defLine.textContent = r["default"] ? "Current default: " + summary(r["default"]) : "No default set - PCs are not managed until you set one.";
    drawTable();
  }).catch(function(e){ tableDiv.replaceChildren(el("p", e.message)); });
}

/* ------------- per-device section in the Actions box ------------- */
function drawDevice(holder, d, r){
  holder.replaceChildren();
  var src = r.source === "device" ? "This PC has its own policy: " : (r.source === "default" ? "Following the default policy: " : "Not managed - no policy applies. ");
  var line = el("p", src + (r.source === "none" ? "" : summary(r.policy)));
  line.style.margin = "4px 0";
  holder.appendChild(line);
  var st = statusCell(r);
  var stLine = el("p", "Status: " + st.textContent, st.className);
  stLine.style.margin = "4px 0";
  holder.appendChild(stLine);
  if(r.applied && r.applied.status === "failed" && r.applied.detail){
    var pre = el("pre", r.applied.detail); pre.style.cssText = "background:#fff4f4;padding:8px;white-space:pre-wrap;margin:4px 0";
    holder.appendChild(pre);
  }
  var boxes = makeChecks(holder);
  setChecks(boxes, r.policy);
  var m = el("span"); m.style.marginLeft = "10px";
  function reload(text, bad){
    api("/admin/devices/" + d.id + "/restrictions").then(function(x){
      if(selected !== d.id) return;
      drawDevice(holder, d, x);
      if(text){ holder.appendChild(el("p", text)); }
    }).catch(function(e){ m.textContent = e.message; m.style.color = "#b00020"; });
  }
  var save = el("button", "Save for this PC"); save.style.marginLeft = "0";
  save.onclick = function(){
    api("/admin/devices/" + d.id + "/restrictions", "POST", readChecks(boxes)).then(function(){ reload(); }).catch(function(e){ m.textContent = e.message; m.style.color = "#b00020"; });
  };
  var inh = el("button", "Follow the default");
  inh.onclick = function(){
    api("/admin/devices/" + d.id + "/restrictions", "POST", {inherit: true}).then(function(){ reload(); }).catch(function(e){ m.textContent = e.message; m.style.color = "#b00020"; });
  };
  holder.appendChild(save); holder.appendChild(inh); holder.appendChild(m);
  var note = el("p", "Untick everything and save to remove this PC's protection. Changes reach online PCs within a minute.");
  note.style.cssText = "color:#555;margin:6px 0 0";
  holder.appendChild(note);
}

var origActions2 = actionsBox;
actionsBox = function(d){
  var b = origActions2(d);
  b.appendChild(el("h3", "Install / uninstall protection"));
  var holder = el("div"); b.appendChild(holder);
  api("/admin/devices/" + d.id + "/restrictions").then(function(r){
    if(selected !== d.id) return;
    drawDevice(holder, d, r);
  }).catch(function(e){ holder.appendChild(el("p", e.message)); });
  return b;
};

$("load").addEventListener("click", refresh);
setInterval(refresh, 10000);
refresh();
})();
</script>`