(function(){
"use strict";
// The viewer of the architecture view (ST-GEN-UC08). It draws each diagram at the places that apply computed, from
// the embedded model alone, and adds selection, filters, the flow stepper and the guided tour. Every text it shows
// comes from the model, from the text forms that apply wrote from the model, or is the fixed text below.
var M = JSON.parse(document.getElementById("model").textContent);
var L = JSON.parse(document.getElementById("layout").textContent);
var $ = function(id){ return document.getElementById(id); };
var NS = "http://www.w3.org/2000/svg";
var SYS = M.system.name;
var LAYER_NAME = {"edge":"Edge","authentication":"Authentication","handler":"Handler","slot":"Slot","repository":"Repository","store":"Store","outside call":"Outside call"};
var LAYER_VAR = {"edge":"edge","authentication":"authentication","handler":"handler","slot":"slot","repository":"repository","store":"store","outside call":"outside"};
var BOUNDARY_STYLE = {"network":"10 5","verified identity":"","tenant scope":"3 3","fill code":"","process":"1 4"};
var GLYPH = {"direct":"●","arrow":"↑","userset":"◎","caveat":"?","intersection":"&","exclusion":"−","wildcard":"*","recursion":"↻","depth limit":"⋯"};
var state = {view:"overview", sel:null, flow:null, step:1, text:false, coll:"", q:""};
var byId = {};

/* ---------- index of every element of the model ---------- */
function put(list){ (list||[]).forEach(function(e){ if (e && e.id) byId[e.id] = e; }); }
[M.containers,M.components,M.mixins,M.ports,M.bindings,M.slots,M.fills,M.entities,M.references,M.collections,M.operations,
 M.actions,M.flows,M.needs,M.boundaries,M.settings,M.authn_points,M.authz_points,M.access.checks,M.access.permissions,M.access.roles].forEach(put);
M.entities.forEach(function(e){ put(e.fields); });
M.access.permissions.forEach(function(p){ put(p.paths); });
if (M.flows.length) state.flow = M.flows[0].id;

function short(id){ var i = String(id).indexOf(":"); return i >= 0 ? String(id).slice(i+1) : String(id); }
function kindOf(id){ var i = String(id).indexOf(":"); return i >= 0 ? String(id).slice(0,i) : ""; }
function nameOf(id){ var e = byId[id]; if (!e) return short(id); if (kindOf(id) === "slot") return short(e.collection)+"."+e.name; if (kindOf(id) === "operation" || kindOf(id) === "flow") return short(id); return e.name || e.label || e.path || short(id); }
function srcText(s){
  if (!s) return "";
  if (s.declaration) return "service.yaml "+s.declaration;
  if (s.registry) return "registry "+s.registry.part+" "+s.registry.name+" "+s.registry.version+(s.registry.field ? ", field "+s.registry.field : "");
  if (s.wiring) return "wiring "+s.wiring;
  if (s.default) return "default, rule "+s.default;
  if (s.rule) return "STEGO rule "+s.rule;
  return "";
}
function aspect(key){ return M.aspects[key] || {state:"present"}; }

/* ---------- what an element connects to, from the relations the model holds ---------- */
function connections(id){
  var out = [], seen = {};
  var push = function(rel, other){ if (other && byId[other] && !seen[rel+"|"+other]){ seen[rel+"|"+other] = 1; out.push({rel:rel, id:other}); } };
  var e = byId[id] || {};
  M.components.forEach(function(c){
    c.ports.forEach(function(p){
      if (c.id === id) push(p.direction === "provides" ? "provides port" : "requires port", p.port);
      if (p.port === id) push(p.direction === "provides" ? "provided by" : "required by", c.id);
    });
    if (c.id === id && c.container) push("runs in", c.container);
    if (c.container === id) push("runs", c.id);
  });
  M.bindings.forEach(function(b){
    if (b.consumer === id || b.provider === id || b.port === id) push("binding", b.id);
    if (b.consumer === id) push("calls, through "+short(b.port), b.provider);
    if (b.provider === id) push("is called by, through "+short(b.port), b.consumer);
    if (b.port === id){ push("bound for consumer", b.consumer); push("bound to provider", b.provider); }
    if (b.id === id){ push("port", b.port); push("consumer", b.consumer); push("provider", b.provider); }
  });
  M.slots.forEach(function(s){
    if (s.offered_by === id) push("offers slot", s.id);
    if (s.id === id){
      push("offered by", s.offered_by); s.fills.forEach(function(f){ push("filled by", f); }); push("on collection", s.collection);
    }
    if (s.fills.indexOf(id) >= 0) push("fills slot", s.id);
    if (s.collection === id) push("has slot", s.id);
  });
  M.fills.forEach(function(f){ if (f.id === id) f.implements.forEach(function(s){ push("implements", s); }); });
  M.actions.forEach(function(a){ if (a.id === id) a.fills.forEach(function(f){ push("filled by", f); }); if (a.fills.indexOf(id) >= 0) push("fills", a.id); });
  M.collections.forEach(function(c){
    if (c.id === id){ push("exposes", c.entity); c.operations.forEach(function(o){ push("offers", o); }); if (c.scope) push("scoped by", c.scope); }
    if (c.entity === id) push("exposed by", c.id);
  });
  M.references.forEach(function(r){
    if (r.from === id || r.to === id || r.field === id) push("reference", r.id);
    if (r.from === id) push("refers to (many to one, via "+short(r.field)+")", r.to);
    if (r.to === id) push("is referred to by (via "+short(r.field)+")", r.from);
    if (r.id === id){ push("from", r.from); push("to", r.to); push("field", r.field); }
  });
  M.entities.forEach(function(en){ en.fields.forEach(function(f){ if (en.id === id) push("has field", f.id); if (f.id === id) push("field of", en.id); }); });
  M.operations.forEach(function(o){ if (o.id === id){ push("on collection", o.collection); push("enters flow", o.flow); } if (o.flow === id) push("operation", o.id); });
  M.needs.forEach(function(n){ if (n.id === id) n.stated_by.forEach(function(c){ push("stated by", c); }); if (n.stated_by.indexOf(id) >= 0) push("states need", n.id); });
  M.boundaries.forEach(function(b){
    if (b.id === id){ b.contains.forEach(function(x){ push("holds", x); }); b.crossed_by.forEach(function(x){ push("crossed by", x); }); }
    if (b.contains.indexOf(id) >= 0) push("inside boundary", b.id);
    if (b.crossed_by.indexOf(id) >= 0) push("crosses boundary", b.id);
  });
  (M.authn_points||[]).concat(M.authz_points||[]).forEach(function(p){ if (p.element === id) push(kindOf(p.id) === "authn" ? "authenticates at" : "authorization at", p.id); if (p.id === id) push("at", p.element); });
  M.access.checks.forEach(function(c){ if (c.subject === id) push("checks", c.id); if (c.id === id){ push("subject", c.subject); push("grants", c.grants); } if (c.grants === id) push("checked by", c.subject); });
  M.access.permissions.forEach(function(p){ p.paths.forEach(function(g){ if (p.id === id) push("grant path", g.id); if (g.id === id){ push("of permission", p.id); push("role", g.role); } if (g.role === id) push("gets", p.id); }); });
  return out;
}
function flowsUsing(id){ return M.flows.filter(function(f){ return f.operation === id || f.steps.some(function(s){ return s.element === id || s.via_port === id; }); }); }

/* ---------- DOM helpers ---------- */
function h(tag, attrs, kids){
  var e = document.createElement(tag);
  if (attrs) for (var k in attrs){ if (k === "text") e.textContent = attrs[k]; else if (k.slice(0,2) === "on") e.addEventListener(k.slice(2), attrs[k]); else e.setAttribute(k, attrs[k]); }
  (kids||[]).forEach(function(c){ if (c != null) e.appendChild(typeof c === "string" ? document.createTextNode(c) : c); });
  return e;
}
function s(tag, attrs, kids){
  var e = document.createElementNS(NS, tag);
  if (attrs) for (var k in attrs){ if (k === "text") e.textContent = attrs[k]; else e.setAttribute(k, attrs[k]); }
  (kids||[]).forEach(function(c){ if (c) e.appendChild(c); });
  return e;
}
function col(layer){ return "var(--"+(LAYER_VAR[layer]||"outside")+")"; }
function narrow(){ return ($("view").clientWidth || window.innerWidth) < 560; }
function pick(pair){ return narrow() ? pair.narrow : pair.wide; }
function svgRoot(w, hgt, label){
  var svg = s("svg", {viewBox:"0 0 "+w+" "+hgt, role:"group", "aria-label":label});
  var d = s("defs");
  d.appendChild(s("marker", {id:"arr", viewBox:"0 0 10 10", refX:"9", refY:"5", markerWidth:"7", markerHeight:"7", orient:"auto-start-reverse"}, [s("path", {d:"M0 0 L10 5 L0 10 z", style:"fill:var(--line)"})]));
  d.appendChild(s("pattern", {id:"hatch", width:"7", height:"7", patternUnits:"userSpaceOnUse", patternTransform:"rotate(45)"}, [s("line", {x1:"0", y1:"0", x2:"0", y2:"7", style:"stroke:var(--slot);stroke-width:2;opacity:.5"})]));
  svg.appendChild(d);
  return svg;
}
function clip(text, max){ text = String(text); return text.length > max ? text.slice(0, max-1)+"…" : text; }
function node(id, p, label, sub, color){
  var g = s("g", {"class":"el", tabindex:"0", role:"button", "data-id":id, "aria-label":label+(sub ? ", "+sub : "")});
  var x = p.x, y = p.y, w = p.w, ht = p.h, box, st = "fill:color-mix(in srgb, "+color+" 16%, var(--panel));stroke:"+color+";stroke-width:2";
  if (p.shape === "store") box = s("path", {"class":"box", d:"M"+x+" "+(y+8)+" a"+(w/2)+" 8 0 0 0 "+w+" 0 v"+(ht-16)+" a"+(w/2)+" 8 0 0 1 -"+w+" 0 z M"+x+" "+(y+8)+" a"+(w/2)+" 8 0 0 1 "+w+" 0", style:st});
  else if (p.shape === "slot"){ var c = 12; box = s("path", {"class":"box", d:"M"+(x+c)+" "+y+" H"+(x+w-c)+" L"+(x+w)+" "+(y+ht/2)+" L"+(x+w-c)+" "+(y+ht)+" H"+(x+c)+" L"+x+" "+(y+ht/2)+" Z", style:st}); }
  else if (p.shape === "entry") box = s("rect", {"class":"box", x:x, y:y, width:w, height:ht, rx:ht/2, style:st+";stroke-dasharray:5 3"});
  else box = s("rect", {"class":"box", x:x, y:y, width:w, height:ht, rx:8, style:st});
  g.appendChild(box);
  var max = Math.floor(w/7), ty = y + (sub ? ht/2 - 3 : ht/2 + 4) + (p.shape === "store" ? 5 : 0);
  g.appendChild(s("text", {x:x+w/2, y:ty, "text-anchor":"middle", "font-weight":"600", text:clip(label, max)}));
  if (sub) g.appendChild(s("text", {"class":"sub", x:x+w/2, y:ty+14, "text-anchor":"middle", text:clip(sub, max+2)}));
  g.appendChild(s("title", {text:label+(sub ? " · "+sub : "")}));
  return g;
}
function line(svg, x1, y1, x2, y2, label, o){
  o = o || {};
  var mx = (x1+x2)/2, my = (y1+y2)/2;
  var d = o.vertical ? "M"+x1+" "+y1+" C"+x1+" "+my+" "+x2+" "+my+" "+x2+" "+y2 : "M"+x1+" "+y1+" C"+mx+" "+y1+" "+mx+" "+y2+" "+x2+" "+y2;
  svg.appendChild(s("path", {"class":"edge-line"+(o.async ? " async" : ""), d:d, "marker-end":"url(#arr)", "data-a":o.a||"", "data-b":o.b||""}));
  if (label) svg.appendChild(s("text", {"class":"sub", x:mx+(o.dx||0), y:my-5, "text-anchor":"middle", text:label}));
}
function connect(svg, a, b, label, o){
  o = o || {};
  if (a.x === b.x || o.vertical){ var down = b.y > a.y; line(svg, a.x+a.w/2, down ? a.y+a.h : a.y, b.x+b.w/2, down ? b.y : b.y+b.h, label, {vertical:true, async:o.async, a:o.a, b:o.b, dx:o.dx}); }
  else if (b.x > a.x) line(svg, a.x+a.w, a.y+a.h/2, b.x, b.y+b.h/2, label, o);
  else line(svg, a.x, a.y+a.h/2, b.x+b.w, b.y+b.h/2, label, o);
}
function posMap(d){ var m = {}; d.nodes.forEach(function(n){ m[n.id] = n; }); return m; }
function emptyCard(reason){ return h("div", {"class":"empty", role:"note"}, [h("strong", {text:"This aspect has no element yet."}), h("p", {text:reason})]); }
function form(view){ return $("form-"+view); }
function cloneForm(view, selector){
  var f = form(view), box = h("div");
  if (!f) return box;
  Array.prototype.forEach.call(f.children, function(c){ if (c.tagName !== "H2" && !(c.classList && c.classList.contains("question"))) box.appendChild(c.cloneNode(true)); });
  if (selector){ var only = h("div"); Array.prototype.forEach.call(box.querySelectorAll(selector), function(x){ only.appendChild(x); }); return only; }
  return box;
}
function setKey(items){
  var k = $("key"); k.textContent = "";
  items.forEach(function(it){
    var sw = null;
    if (it[0] === "box") sw = h("span", {"class":"sw", style:"border-color:"+col(it[1])+";background:color-mix(in srgb,"+col(it[1])+" 16%,var(--panel))"});
    else if (it[0] === "hex") sw = h("span", {"class":"sw", style:"border-color:"+col(it[1])+";clip-path:polygon(25% 0,75% 0,100% 50%,75% 100%,25% 100%,0 50%);background:"+col(it[1])});
    else if (it[0] === "store") sw = h("span", {"class":"sw", style:"border-radius:6px/3px;border-color:"+col(it[1])});
    else if (it[0] === "entry") sw = h("span", {"class":"sw", style:"border-radius:6px;border-style:dashed;border-color:"+col(it[1])});
    k.appendChild(h("li", {}, [sw, it[2]]));
  });
}
function layerKey(){ M.layers.forEach(function(l){ $("key").appendChild(h("li", {}, [h("span", {"class":"sw", style:"border-color:"+col(l)+";background:"+col(l)}), "column "+(M.layers.indexOf(l)+1)+": "+LAYER_NAME[l]])); }); }

/* ---------- the six views ---------- */
var VIEWS = [
  {id:"overview", nav:"Overview", title:"Context of", aspect:"structure", q:"What does this service talk to, and what runs where?", render:renderOverview},
  {id:"components", nav:"Components", title:"Components of", aspect:"structure", q:"What is the service made of, and where does each part sit in a request?", render:renderComponents},
  {id:"data", nav:"Data model", title:"Data model of", aspect:"data_model", q:"What data does it hold, how does it relate, and what may callers do with it?", render:renderData},
  {id:"flows", nav:"Request flows", title:"Request flows of", aspect:"request_flows", q:"What happens, step by step, when someone calls an operation, and where can it stop?", render:renderFlows},
  {id:"trust", nav:"Trust and tenancy", title:"Trust and tenancy of", aspect:"trust_and_tenancy", q:"Where is a caller trusted, where is the tenant fixed, which code is owner code, and who may do what?", render:renderTrust},
  {id:"platform", nav:"Platform edges", title:"Platform edges of", aspect:"platform_edges", q:"What must the platform supply for this service to run?", render:renderPlatform}
];

function subOf(id){
  var e = byId[id], k = kindOf(id);
  if (k === "container") return e.kind === "service" ? M.system.archetype+" "+M.system.archetype_version+" · "+M.system.language : e.kind;
  if (k === "collection") return "entry · "+short(e.entity);
  if (k === "component") return e.version;
  if (k === "slot") return e.name;
  return "";
}
function labelOf(id){ var e = byId[id]; return kindOf(id) === "slot" ? short(e.collection) : nameOf(id); }

function renderOverview(root){
  var d = pick(L.overview), P = posMap(d), svg = svgRoot(d.w, d.h, "Context diagram of "+SYS);
  (d.regions||[]).forEach(function(r){ r.rects.forEach(function(rc){
    svg.appendChild(s("rect", {x:rc[0], y:rc[1], width:rc[2], height:rc[3], rx:14, style:"fill:none;stroke:var(--edge);stroke-width:2;stroke-dasharray:10 5"}));
    svg.appendChild(s("text", {x:rc[0]+10, y:rc[1]+16, "font-weight":"700", text:byId[r.id].label}));
  }); });
  (d.edges||[]).forEach(function(e){ var a = P[e.from], b = P[e.to]; if (a && b) connect(svg, a, b, e.label, {a:e.from, b:e.to, vertical:narrow(), dx:narrow() ? 60 : 0}); });
  d.nodes.forEach(function(n){ svg.appendChild(node(n.id, n, labelOf(n.id), subOf(n.id), n.shape === "entry" ? col("edge") : col(n.layer))); });
  root.appendChild(h("div", {"class":"canvas"}, [svg]));
  setKey([["entry","edge","Entry: a collection that callers reach through the network edge"],["box","handler","Container: the service process"],["store","store","Container that a need states"],["line","","Line: a call into the service, or a store it uses"]]);
}

function chips(){
  var tb = $("toolbar"), names = [];
  M.slots.forEach(function(sl){ var c = short(sl.collection); if (names.indexOf(c) < 0) names.push(c); });
  if (!names.length) return;
  tb.appendChild(h("span", {"class":"k", text:"Slots of:"}));
  [""].concat(names).forEach(function(c){
    tb.appendChild(h("button", {"class":"chip", type:"button", "aria-pressed":String(state.coll === c), text:c || "every collection", onclick:function(){ state.coll = c; render(); }}));
  });
}
function drawLanes(svg, d){
  d.lanes.forEach(function(l, i){
    svg.appendChild(s("rect", {x:l.x, y:l.y, width:l.w, height:l.h, rx:10, style:"fill:color-mix(in srgb, "+col(l.layer)+" 6%, transparent);stroke:var(--rule)"}));
    svg.appendChild(s("text", {"class":"lane-label", x:l.x+8, y:l.y+16, text:(i+1)+" "+LAYER_NAME[l.layer]}));
  });
}
function drawComponents(svg, d, ctx){
  var P = posMap(d), n = narrow();
  M.bindings.forEach(function(b){ var a = P[b.consumer], c = P[b.provider]; if (a && c) connect(svg, a, c, n ? "" : short(b.port), {a:b.consumer, b:b.provider}); });
  M.slots.forEach(function(sl){ var a = P[sl.id], c = P[sl.offered_by]; if (!a || !c || n) return;
    svg.appendChild(s("path", {"class":"edge-line", d:"M"+(c.x+c.w)+" "+(c.y+c.h/2)+" L"+a.x+" "+(a.y+a.h/2), style:"stroke-dasharray:1 3", "data-a":sl.offered_by, "data-b":sl.id})); });
  d.nodes.forEach(function(p){
    var g = node(p.id, p, labelOf(p.id), subOf(p.id), col(p.layer));
    if (ctx) g.classList.add("ctx");
    if (state.coll && p.shape === "slot" && short(byId[p.id].collection) !== state.coll) g.classList.add("dim", "filtered");
    svg.appendChild(g);
  });
  M.components.forEach(function(c){
    var p = P[c.id]; if (!p) return;
    var ins = c.ports.filter(function(x){ return x.side === "inbound"; }), outs = c.ports.filter(function(x){ return x.side === "outbound"; });
    [[ins, p.x-5], [outs, p.x+p.w-5]].forEach(function(pair){
      pair[0].forEach(function(pt, i){
        var r = s("rect", {"class":"port", x:pair[1], y:p.y+6+i*10, width:10, height:8, "data-port":pt.port, style:"fill:"+(pt.direction === "provides" ? "var(--ink)" : "var(--panel)")});
        r.appendChild(s("title", {text:short(pt.port)+": "+pt.direction+", "+pt.side}));
        r.addEventListener("click", function(ev){ ev.stopPropagation(); select(pt.port); });
        svg.appendChild(r);
      });
    });
  });
}
function renderComponents(root){
  chips();
  var d = pick(L.components), svg = svgRoot(d.w, d.h, "Components of "+SYS+" in layer lanes");
  drawLanes(svg, d); drawComponents(svg, d, false);
  root.appendChild(h("div", {"class":"canvas"}, [svg]));
  setKey([["box","handler","Box: a component"],["hex","slot","Hexagon: a slot, where owner fills run"],["store","store","Cylinder: a store"],["line","","■ provided port, □ required port; left inbound, right outbound; line: consumer to provider"]]);
  layerKey();
}

function renderData(root){
  var d = pick(L.data), P = posMap(d), svg = svgRoot(d.w, d.h, "Entity relationship diagram of "+SYS), n = narrow();
  M.references.forEach(function(r){
    var a = P[r.from], b = P[r.to], e = byId[r.from]; if (!a || !b) return;
    var i = e.fields.map(function(f){ return f.id; }).indexOf(r.field), ya = a.y+30+i*18+12-4, yb = b.y+14, path;
    if (n || a.x === b.x){ var xr = a.x+a.w; path = "M"+xr+" "+ya+" C"+(xr+40)+" "+ya+" "+(xr+40)+" "+yb+" "+xr+" "+yb; }
    else path = "M"+a.x+" "+ya+" C"+(a.x-70)+" "+ya+" "+(b.x+b.w+70)+" "+yb+" "+(b.x+b.w)+" "+yb;
    svg.appendChild(s("path", {"class":"edge-line", d:path, "data-a":r.from, "data-b":r.to}));
    var sx = (n || a.x === b.x) ? a.x+a.w : a.x, dir = (n || a.x === b.x) ? 1 : -1;
    svg.appendChild(s("path", {"class":"edge-line", d:"M"+(sx+dir*10)+" "+ya+" L"+sx+" "+(ya-6)+" M"+(sx+dir*10)+" "+ya+" L"+sx+" "+(ya+6)+" M"+(sx+dir*10)+" "+ya+" L"+sx+" "+ya, "data-a":r.from, "data-b":r.to}));
  });
  M.entities.forEach(function(e){
    var p = P[e.id]; if (!p) return;
    var g = s("g", {"class":"el", tabindex:"0", role:"button", "data-id":e.id, "aria-label":"Entity "+e.name+", "+e.fields.length+" fields, tenant "+e.tenant_kind});
    g.appendChild(s("rect", {"class":"box", x:p.x, y:p.y, width:p.w, height:p.h, rx:8, style:"fill:var(--panel);stroke:var(--repository);stroke-width:2"}));
    g.appendChild(s("rect", {x:p.x+1, y:p.y+1, width:p.w-2, height:23, rx:7, style:"fill:color-mix(in srgb, var(--repository) 18%, var(--panel))"}));
    g.appendChild(s("text", {x:p.x+10, y:p.y+17, "font-weight":"700", text:e.name}));
    g.appendChild(s("text", {"class":"sub", x:p.x+p.w-10, y:p.y+17, "text-anchor":"end", text:"tenant "+e.tenant_kind}));
    e.fields.forEach(function(f, i){
      var y = p.y+30+i*18+12, ref = M.references.filter(function(r){ return r.field === f.id; })[0];
      g.appendChild(s("text", {x:p.x+10, y:y, text:(ref ? "→ " : "")+f.name+(f.required ? " *" : "")}));
      g.appendChild(s("text", {"class":"sub", x:p.x+140, y:y, text:clip(f.type+(f.sensitivity !== "public" ? " · "+f.sensitivity : "")+(ref ? " · "+short(ref.to) : ""), 24)}));
    });
    var colls = M.collections.filter(function(c){ return c.entity === e.id; }).map(function(c){ return c.name; });
    g.appendChild(s("text", {"class":"sub", x:p.x+10, y:p.y+p.h-9, text:clip("Exposed by: "+(colls.join(", ") || "none"), 46)}));
    svg.appendChild(g);
  });
  root.appendChild(h("div", {"class":"canvas"}, [svg]));
  var mx = cloneForm("data", "table.matrix");
  root.appendChild(h("h3", {text:"Operations each collection offers"}));
  root.appendChild(h("p", {"class":"question", text:"Select a ● to open the request flow of that operation."}));
  var t = mx.querySelector("table"); if (t) root.appendChild(h("div", {"class":"scroll"}, [t]));
  Array.prototype.forEach.call(root.querySelectorAll("td[data-op]"), function(td){
    var op = td.getAttribute("data-op"), b = h("button", {"class":"linkish", type:"button", "aria-label":"Open the flow of "+short(op), text:"●", onclick:function(ev){ ev.stopPropagation(); openFlow(byId[op] ? byId[op].flow : null, 1); }});
    td.textContent = ""; td.appendChild(b);
  });
  setKey([["box","repository","Box: an entity with its fields; * required, → reference"],["line","","Line with a crow's foot: many rows refer to one"]]);
}

function stepLane(f, i){ var l = L.flows.steps[f.id]; return l ? l[i] : 0; }
function openFlow(id, step){ if (!id || !byId[id]) return; state.view = "flows"; state.flow = id; state.step = step || 1; state.sel = byId[id].steps[(step||1)-1].element; render(); }
function opLabel(f){ var o = byId[f.operation]; return o ? (o.method ? o.method+" " : "")+short(o.collection)+" · "+o.verb : short(f.id); }
function renderFlows(root){
  var mini = h("div", {"class":"mini", role:"group", "aria-label":"All request flows, one for each operation"});
  M.flows.forEach(function(f){
    var b = h("button", {type:"button", "aria-pressed":String(f.id === state.flow), "data-flow":f.id, "aria-label":opLabel(f)+", "+f.steps.length+" steps, "+f.exits.length+" exits", onclick:function(){ openFlow(f.id, 1); }});
    b.appendChild(h("span", {"class":"t", text:opLabel(f)}));
    var sv = s("svg", {viewBox:"0 0 160 14", "aria-hidden":"true"});
    f.steps.forEach(function(st, i){ sv.appendChild(s("circle", {cx:6+i*12, cy:7, r:4.5, style:"fill:"+col(M.layers[stepLane(f, i)])+";stroke:var(--ink);stroke-width:"+(st.mode === "async" ? 1.2 : 0)})); });
    b.appendChild(sv);
    b.appendChild(h("span", {"class":"k", text:f.steps.length+" steps · "+f.exits.length+(f.exits.length === 1 ? " exit" : " exits")}));
    mini.appendChild(b);
  });
  root.appendChild(h("h3", {text:"All flows"}));
  root.appendChild(mini);
  var f = byId[state.flow]; if (!f) return;
  var n = f.steps.length; state.step = Math.min(Math.max(state.step, 1), n);
  var o = byId[f.operation];
  root.appendChild(h("h3", {id:"flowTitle", text:"Step by step: "+(o && o.method ? o.method+" "+o.path : short(f.operation))}));
  root.appendChild(h("div", {"class":"stepper"}, [
    h("button", {"class":"btn", type:"button", id:"prevStep", text:"◀ Previous", onclick:function(){ go(-1); }}),
    h("button", {"class":"btn", type:"button", id:"nextStep", text:"Next ▶", onclick:function(){ go(1); }}),
    h("span", {id:"stepOf", text:"Step "+state.step+" of "+n})
  ]));
  if (!narrow()){
    var lw = L.flows.lane_w, W = L.flows.w, rh = 44, H = n*rh+44, svg = svgRoot(W, H, "Lane of each step of "+opLabel(f));
    M.layers.forEach(function(l, i){
      svg.appendChild(s("line", {x1:lw*i+lw/2, y1:30, x2:lw*i+lw/2, y2:H-4, style:"stroke:var(--rule);stroke-width:1"}));
      svg.appendChild(s("text", {"class":"lane-label", x:lw*i+lw/2, y:16, "text-anchor":"middle", text:LAYER_NAME[l]}));
    });
    f.steps.forEach(function(st, i){
      var x = lw*stepLane(f, i)+lw/2, y = 44+i*rh;
      if (i > 0){ var px = lw*stepLane(f, i-1)+lw/2, py = 44+(i-1)*rh;
        svg.appendChild(s("path", {"class":"edge-line"+(st.mode === "async" ? " async" : "")+(i < state.step ? " hot" : ""), d:"M"+px+" "+(py+9)+" L"+x+" "+(y-10), "marker-end":"url(#arr)"})); }
      var cur = (i+1) === state.step, done = i < state.step;
      svg.appendChild(s("circle", {cx:x, cy:y, r:cur ? 12 : 10, style:"fill:"+(done ? "color-mix(in srgb, "+col(M.layers[stepLane(f, i)])+" 35%, var(--panel))" : "var(--panel)")+";stroke:"+(cur ? "var(--ink)" : col(M.layers[stepLane(f, i)]))+";stroke-width:"+(cur ? 3 : 2)}));
      svg.appendChild(s("text", {x:x, y:y+4, "text-anchor":"middle", "font-size":"11", "font-weight":"700", text:String(i+1)}));
      f.exits.filter(function(e){ return e.after_step === i+1; }).forEach(function(e, k){
        svg.appendChild(s("path", {"class":"edge-line", d:"M"+(x+12)+" "+y+" l26 "+(8+k*12), style:"stroke-dasharray:2 2", "marker-end":"url(#arr)"}));
        svg.appendChild(s("text", {"class":"sub", x:x+42, y:y+12+k*12, text:clip(e.result, 26)}));
      });
    });
    root.appendChild(h("div", {"class":"canvas"}, [svg]));
  }
  var ol = h("ol", {"class":"steps", "aria-label":"Steps of "+opLabel(f)});
  f.steps.forEach(function(st, i){
    var cur = (i+1) === state.step, li = h("li", {"class":cur ? "cur" : ""});
    var btn = h("button", {type:"button", "data-step":String(i+1), "data-id":st.element, "aria-current":cur ? "step" : "false", onclick:function(){ state.step = i+1; state.sel = st.element; render(); focusStep(); }});
    btn.appendChild(h("span", {"class":"k", text:st.kind+" · "+LAYER_NAME[M.layers[stepLane(f, i)]]+(st.mode === "async" ? " · later" : "")+(st.via_port ? " · port "+short(st.via_port) : "")}));
    btn.appendChild(h("span", {}, [h("span", {"class":"n", text:(i+1)+"."}), nameOf(st.element)+": "+st.text]));
    f.exits.filter(function(e){ return e.after_step === i+1; }).forEach(function(e){ btn.appendChild(h("span", {"class":"exit", text:"↳ stops: "+e.result+", when "+e.when})); });
    li.appendChild(btn); ol.appendChild(li);
  });
  root.appendChild(ol);
  var st = f.steps[state.step-1];
  if (!state.sel) state.sel = st.element;
  $("live").textContent = "Step "+state.step+" of "+n+": "+nameOf(st.element)+". "+st.text;
  setKey([["line","","Dot column: the layer; filled: done; dashed arrow: later; ↳ stops: where the call can stop, and its result"]]);
}
function go(dir){
  var f = byId[state.flow]; if (!f) return;
  var nx = state.step+dir; if (nx < 1 || nx > f.steps.length) return;
  state.step = nx; state.sel = f.steps[nx-1].element; render(); focusStep();
}
function focusStep(){ var b = document.querySelector(".steps li.cur button"); if (b) b.focus(); }

function renderTrust(root){
  var d = pick(L.components), P = posMap(d), svg = svgRoot(d.w, d.h, "Trust boundaries of "+SYS+" over the component lanes");
  drawLanes(svg, d);
  (d.regions||[]).forEach(function(r, i){
    var b = byId[r.id], dash = BOUNDARY_STYLE[b.kind] || "", inset = (i%3)*5, colr = "var(--ink)";
    var g = s("g", {"class":"el", tabindex:"0", role:"button", "data-id":r.id, "aria-label":"Boundary: "+b.label});
    r.rects.forEach(function(rc){
      var fill = b.kind === "fill code" ? "url(#hatch)" : "none";
      g.appendChild(s("rect", {"class":"box", x:rc[0]+inset, y:rc[1]+inset, width:rc[2]-2*inset, height:rc[3]-2*inset, rx:12, style:"fill:"+fill+";stroke:"+colr+";stroke-width:2;stroke-dasharray:"+(dash || "none")}));
      g.appendChild(s("text", {x:rc[0]+inset+8, y:rc[1]+rc[3]-inset-6, "font-weight":"700", text:b.kind}));
    });
    r.outlines.forEach(function(id){ var p = P[id]; if (!p) return;
      g.appendChild(s("rect", {"class":"box", x:p.x-4-inset/2, y:p.y-4-inset/2, width:p.w+8+inset, height:p.h+8+inset, rx:10, style:"fill:"+(b.kind === "fill code" ? "url(#hatch)" : "none")+";stroke:"+colr+";stroke-width:2;stroke-dasharray:"+(dash || "none")}));
    });
    g.appendChild(s("title", {text:b.kind+": "+b.label}));
    svg.appendChild(g);
  });
  drawComponents(svg, d, true);
  root.appendChild(h("div", {"class":"canvas"}, [svg]));
  var cards = h("div", {"class":"cards", role:"group", "aria-label":"Trust boundaries"});
  M.boundaries.forEach(function(b){
    var drawn = (d.regions||[]).some(function(r){ return r.id === b.id; });
    cards.appendChild(h("button", {type:"button", "data-id":b.id, onclick:function(){ select(b.id); }}, [
      h("strong", {text:b.kind}), h("span", {"class":"k", text:b.label}),
      h("span", {"class":"k", text:"Holds "+b.contains.length+", crossed by "+b.crossed_by.length+(drawn ? "" : "; not drawn above, because it holds no element of the lanes")})]));
  });
  root.appendChild(h("h3", {text:"Trust boundaries"}));
  root.appendChild(cards);
  var parts = cloneForm("trust");
  var hs = parts.querySelectorAll("h3");
  // Tenancy, the points of authentication and authorization, and the access matrix, from the text form.
  Array.prototype.forEach.call(hs, function(hh){
    if (/^Trust boundaries/.test(hh.textContent) || /^Every setting/.test(hh.textContent)) return;
    root.appendChild(hh.cloneNode(true));
    var n = hh.nextElementSibling;
    while (n && n.tagName !== "H3"){ root.appendChild(n.cloneNode(true)); n = n.nextElementSibling; }
  });
  Array.prototype.forEach.call(root.querySelectorAll("button.cell"), function(b){
    var id = "cell:"+b.getAttribute("data-check")+"|"+b.getAttribute("data-role");
    b.setAttribute("aria-pressed", String(state.sel === id));
    b.addEventListener("click", function(ev){ ev.stopPropagation(); select(id); });
  });
  setKey([["line","","Long dashes: network edge; solid: verified identity; short dashes: tenant scope; hatched: fill code; dotted: process"]]);
}

function renderPlatform(root){ root.appendChild(cloneForm("platform")); setKey([]); }

/* ---------- selection and details ---------- */
function select(id){ state.sel = id; writeHash(); paintSel(); details(); }
function paintSel(){
  var con = state.sel && byId[state.sel] ? connections(state.sel).map(function(c){ return c.id; }) : [];
  Array.prototype.forEach.call(document.querySelectorAll("#view .el"), function(g){
    var id = g.getAttribute("data-id");
    g.classList.toggle("sel", id === state.sel);
    var match = !state.q || nameOf(id).toLowerCase().indexOf(state.q.toLowerCase()) >= 0 || id.toLowerCase().indexOf(state.q.toLowerCase()) >= 0;
    g.classList.toggle("dim", g.classList.contains("filtered") || !match || (!!state.sel && byId[state.sel] && id !== state.sel && con.indexOf(id) < 0 && state.view !== "trust"));
  });
  Array.prototype.forEach.call(document.querySelectorAll("#view .edge-line[data-a]"), function(p){
    var a = p.getAttribute("data-a"), b = p.getAttribute("data-b"), on = state.sel && (a === state.sel || b === state.sel);
    if (a){ p.classList.toggle("hot", !!on); p.classList.toggle("dim", !!state.sel && !!byId[state.sel] && !on); }
  });
  Array.prototype.forEach.call(document.querySelectorAll("#view tr[data-id]"), function(tr){ tr.classList.toggle("sel", tr.getAttribute("data-id") === state.sel); });
  Array.prototype.forEach.call(document.querySelectorAll("#view button.cell"), function(b){ b.setAttribute("aria-pressed", String(state.sel === "cell:"+b.getAttribute("data-check")+"|"+b.getAttribute("data-role"))); });
}
function valueText(v){
  if (v === null || v === undefined) return "";
  if (Array.isArray(v)) return v.map(function(x){ return typeof x === "object" ? "" : (byId[x] ? nameOf(x) : String(x)); }).filter(Boolean).join(", ");
  if (typeof v === "object") return "";
  return typeof v === "string" && byId[v] ? nameOf(v) : String(v);
}
function grantSentence(p, g){
  var t = (g.role ? short(g.role) : "no role")+" ["+g.kinds.join(", ")+"] via "+g.hops.join(" → ");
  if (g.caveat) t += "; only when caveat "+g.caveat+" holds";
  if (g.requires && g.requires.length) t += "; only together with "+g.requires.join(", ");
  if (g.except && g.except.length) t += "; not where "+g.except.join(", ");
  if (g.stops) t += "; stops: "+g.stops;
  return t;
}
function cellDetails(d, id){
  var parts = id.slice(5).split("|"), c = byId[parts[0]], role = parts[1], p = c && byId[c.grants];
  d.appendChild(h("span", {"class":"type", text:"access matrix cell"}));
  d.appendChild(h("h3", {text:(role ? short(role) : "Paths that stop")+" · "+short(c.subject)}));
  var dl = h("dl");
  [["Operation", nameOf(c.subject)], ["Checks", c.resource_type+"#"+c.permission], ["Source", srcText(c.source)]].forEach(function(r){ dl.appendChild(h("dt", {text:r[0]})); dl.appendChild(h("dd", {text:r[1]})); });
  d.appendChild(dl);
  d.appendChild(h("strong", {text:"Grant paths"}));
  var ul = h("ul");
  (p ? p.paths : []).filter(function(g){ return (g.role || "") === role; }).forEach(function(g){
    ul.appendChild(h("li", {"data-grant":g.id}, [g.kinds.map(function(k){ return GLYPH[k] || ""; }).join("")+" "+grantSentence(p, g)]));
  });
  d.appendChild(ul);
  d.appendChild(h("p", {"class":"k", text:"Grant paths come from the declared schema "+(M.access.schema || "")+". The view shows no relationship of a real user."}));
}
function details(){
  var d = $("details"); d.textContent = "";
  var id = state.sel;
  if (id && id.indexOf("cell:") === 0 && byId[id.slice(5).split("|")[0]]){ d.classList.remove("closed"); closeBtn(d); cellDetails(d, id); return; }
  var e = id && byId[id];
  if (!e){ d.appendChild(h("p", {"class":"question", text:"Select any box, row, step or cell to see what it is, where it comes from, and what it connects to."})); d.classList.add("closed"); return; }
  d.classList.remove("closed");
  closeBtn(d);
  d.appendChild(h("span", {"class":"type", text:kindOf(id)}));
  d.appendChild(h("h3", {id:"detailsTitle", text:nameOf(id)}));
  var dl = h("dl");
  Object.keys(e).forEach(function(k){
    if (k === "id" || k === "source" || k === "steps" || k === "exits" || k === "fields" || k === "paths" || k === "facts") return;
    var v = k === "ports" ? e.ports.map(function(p){ return short(p.port)+" ("+p.direction+", "+p.side+")"; }).join("; ")
          : k === "offers" ? e.offers.map(function(o){ return o.name+" ("+o.phase.replace("_", " ")+": "+o.operations.join(", ")+")"; }).join("; ")
          : (k === "layer" ? LAYER_NAME[e.layer] : valueText(e[k]));
    if (v === "") return;
    dl.appendChild(h("dt", {text:k.replace(/_/g, " ")})); dl.appendChild(h("dd", {text:v}));
  });
  dl.appendChild(h("dt", {text:"source"})); dl.appendChild(h("dd", {"data-source":"1", text:srcText(e.source) || "the model"}));
  d.appendChild(dl);
  var con = connections(id);
  if (con.length){
    d.appendChild(h("strong", {text:"Connects to"}));
    var ul = h("ul", {"class":"conn"});
    con.forEach(function(c){ ul.appendChild(h("li", {"data-id":c.id}, [c.rel+" ", h("button", {"class":"linkish", type:"button", text:nameOf(c.id), onclick:function(){ select(c.id); }})])); });
    d.appendChild(ul);
  }
  var fl = flowsUsing(id);
  if (fl.length && kindOf(id) !== "flow"){
    d.appendChild(h("strong", {text:"Used in "+fl.length+" request flow"+(fl.length > 1 ? "s" : "")}));
    var ul2 = h("ul");
    fl.forEach(function(f){ var at = 1+f.steps.map(function(x){ return x.element; }).indexOf(id); ul2.appendChild(h("li", {"data-flow":f.id}, [h("button", {"class":"linkish", type:"button", text:opLabel(f), onclick:function(){ openFlow(f.id, at > 0 ? at : 1); }})])); });
    d.appendChild(ul2);
  }
}
function closeBtn(d){ d.appendChild(h("div", {style:"display:flex;justify-content:flex-end"}, [h("button", {"class":"linkish", type:"button", text:"Close details", onclick:function(){ state.sel = null; writeHash(); paintSel(); details(); }})])); }

/* ---------- navigation, hash and render ---------- */
function buildNav(){
  var ul = $("nav"); ul.textContent = "";
  VIEWS.forEach(function(v){
    ul.appendChild(h("li", {}, [h("button", {type:"button", "data-view":v.id, "aria-current":state.view === v.id ? "page" : "false", onclick:function(){ state.view = v.id; state.sel = null; state.text = false; render(); $("vtitle").focus(); }}, [v.nav, h("span", {"class":"q", text:v.q})])]));
  });
}
function writeHash(){
  var p = "view="+state.view;
  if (state.view === "flows" && state.flow) p += "&flow="+encodeURIComponent(short(state.flow))+"&step="+state.step;
  if (state.sel) p += "&sel="+encodeURIComponent(state.sel);
  if (state.coll) p += "&coll="+encodeURIComponent(state.coll);
  if (state.text) p += "&text=1";
  if (location.hash.slice(1) !== p){ try { history.replaceState(null, "", "#"+p); } catch(_){ location.hash = p; } }
}
function readHash(){
  var q = {};
  location.hash.slice(1).split("&").forEach(function(kv){ var i = kv.indexOf("="); if (i > 0){ try { q[kv.slice(0,i)] = decodeURIComponent(kv.slice(i+1)); } catch(_){} } });
  if (q.view && VIEWS.some(function(v){ return v.id === q.view; })) state.view = q.view;
  if (q.flow && byId["flow:"+q.flow]) state.flow = "flow:"+q.flow;
  if (q.step) state.step = Math.max(1, parseInt(q.step, 10) || 1);
  state.sel = q.sel && (byId[q.sel] || q.sel.indexOf("cell:") === 0) ? q.sel : null;
  state.coll = q.coll || "";
  state.text = q.text === "1";
}
function render(){
  var v = VIEWS.filter(function(x){ return x.id === state.view; })[0];
  buildNav();
  $("vtitle").textContent = v.title+" "+SYS; $("vq").textContent = v.q;
  var tb = $("toolbar"); tb.textContent = "";
  var root = $("view"); root.textContent = ""; $("key").textContent = "";
  var a = aspect(v.aspect);
  if (a.state === "empty"){ root.appendChild(emptyCard(a.reason)); }
  else {
    if (v.id !== "platform") tb.appendChild(h("button", {"class":"chip", type:"button", id:"textToggle", "aria-pressed":String(state.text), text:"Show as text", onclick:function(){ state.text = !state.text; render(); }}));
    if (!state.text && v.id !== "platform"){
      var inp = h("input", {type:"search", id:"filter", "aria-label":"Filter by name", placeholder:"Filter by name", value:state.q});
      inp.addEventListener("input", function(){ state.q = inp.value; paintSel(); });
      tb.appendChild(inp);
    }
    if (state.text){ root.appendChild(cloneForm(v.id)); }
    else v.render(root);
  }
  wire(); paintSel(); details(); writeHash();
}
function wire(){
  Array.prototype.forEach.call(document.querySelectorAll("#view .el"), function(g){
    g.addEventListener("click", function(){ select(g.getAttribute("data-id")); });
    g.addEventListener("keydown", function(e){
      if (e.key === "Enter" || e.key === " "){ e.preventDefault(); select(g.getAttribute("data-id")); }
      if (["ArrowRight","ArrowLeft","ArrowUp","ArrowDown"].indexOf(e.key) >= 0 && state.view !== "flows"){ e.preventDefault(); e.stopPropagation(); neighbour(g, e.key); }
    });
  });
  Array.prototype.forEach.call(document.querySelectorAll("#view tr[data-id]"), function(tr){
    var id = tr.getAttribute("data-id"); if (!byId[id]) return;
    tr.tabIndex = 0;
    tr.addEventListener("click", function(){ select(id); });
    tr.addEventListener("keydown", function(e){ if (e.key === "Enter" || e.key === " "){ e.preventDefault(); select(id); } });
  });
}
// neighbour moves the focus to the nearest element of the same diagram in the direction of the arrow key.
function neighbour(g, key){
  var r0 = g.getBoundingClientRect(), best = null, bd = 1e9;
  Array.prototype.forEach.call(g.ownerSVGElement.querySelectorAll(".el"), function(o){
    if (o === g) return;
    var r = o.getBoundingClientRect(), dx = (r.left+r.width/2)-(r0.left+r0.width/2), dy = (r.top+r.height/2)-(r0.top+r0.height/2);
    var ok = key === "ArrowRight" ? dx > 1 : key === "ArrowLeft" ? dx < -1 : key === "ArrowDown" ? dy > 1 : dy < -1;
    if (!ok) return;
    var main = (key === "ArrowRight" || key === "ArrowLeft") ? Math.abs(dx) : Math.abs(dy), side = (key === "ArrowRight" || key === "ArrowLeft") ? Math.abs(dy) : Math.abs(dx);
    var dist = main+2*side; if (dist < bd){ bd = dist; best = o; }
  });
  if (best) best.focus();
}
document.addEventListener("keydown", function(e){
  var t = e.target && e.target.tagName;
  if (t === "INPUT") return;
  if (state.view === "flows" && !state.text && (e.key === "ArrowRight" || e.key === "ArrowLeft")){ e.preventDefault(); go(e.key === "ArrowRight" ? 1 : -1); }
  if (e.key === "/" && $("filter")){ e.preventDefault(); $("filter").focus(); }
  if (e.key === "Escape"){ if (tour.el) closeTour(); else if (state.sel){ state.sel = null; writeHash(); paintSel(); details(); } }
});
window.addEventListener("hashchange", function(){ readHash(); render(); });
var rz, lastNarrow = null;
window.addEventListener("resize", function(){ clearTimeout(rz); rz = setTimeout(function(){ var n = narrow(); if (n !== lastNarrow){ lastNarrow = n; render(); } }, 120); });

/* ---------- theme, record and download ---------- */
function isDark(){ var cur = document.documentElement.getAttribute("data-theme"); return cur ? cur === "dark" : !!(window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches); }
function themeLabel(){ $("themeBtn").textContent = isDark() ? "Light theme" : "Dark theme"; }
try { var saved = localStorage.getItem("stego-arch-theme"); if (saved === "dark" || saved === "light") document.documentElement.setAttribute("data-theme", saved); } catch(_){}
$("themeBtn").addEventListener("click", function(){
  var t = isDark() ? "light" : "dark";
  document.documentElement.setAttribute("data-theme", t);
  try { localStorage.setItem("stego-arch-theme", t); } catch(_){}
  themeLabel();
});
themeLabel();
$("dlBtn").setAttribute("href", "data:application/json;charset=utf-8,"+encodeURIComponent(document.getElementById("model").textContent));

/* ---------- the guided tour (ST-GEN-UC08-R09) ---------- */
var tour = {i:0, el:null};
function tourFlow(){
  var gate = null, exitAfterSlot = null, longest = null;
  M.flows.forEach(function(f){
    f.steps.forEach(function(st, i){
      var sl = st.kind === "slot" && byId[st.element];
      if (sl && sl.strategy === "gate" && !gate) gate = {flow:f, step:i+1};
      if (sl && !exitAfterSlot && f.exits.some(function(x){ return x.after_step === i+1; })) exitAfterSlot = {flow:f, step:i+1};
    });
    if (!longest || f.steps.length > longest.flow.steps.length) longest = {flow:f, step:1};
  });
  return gate || exitAfterSlot || longest;
}
function stops(){
  var list = [], handler = M.components.filter(function(c){ return c.layer === "handler"; })[0] || M.components[0];
  var terms = [h("dt", {text:"Component"}), h("dd", {text:"A part STEGO generates"+(handler ? ", such as "+handler.name : "")+"."}),
    h("dt", {text:"Layer"}), h("dd", {text:"Where a part sits in a request, left to right: "+M.layers.map(function(l){ return LAYER_NAME[l].toLowerCase(); }).join(", ")+"."}),
    h("dt", {text:"Port"}), h("dd", {text:"A named socket. One part provides it, another requires it"+(M.bindings[0] ? ", such as "+short(M.bindings[0].port) : "")+"."}),
    h("dt", {text:"Slot"}), h("dd", {text:"A place in a request where the owner's own code may run"+(M.slots[0] ? ", such as "+nameOf(M.slots[0].id) : "")+"."}),
    h("dt", {text:"Fill"}), h("dd", {text:"The owner's code in a slot"+(M.fills[0] ? ", such as "+M.fills[0].name : "")+"."})];
  list.push({title:"Five words first", body:function(){ return h("dl", {}, terms.map(function(x){ return x.cloneNode(true); })); }});
  if (aspect("structure").state === "present"){
    var entries = L.overview.wide.nodes.filter(function(n){ return n.shape === "entry"; }).map(function(n){ return nameOf(n.id); });
    var svc = M.containers.filter(function(c){ return c.kind === "service"; })[0];
    list.push({title:"1. The big picture", view:"overview", sel:svc ? svc.id : null, text:"Callers reach "+SYS+" through the network edge, by "+entries.length+" collection"+(entries.length === 1 ? "" : "s")+(entries.length ? ": "+entries.join(", ") : "")+". Select a box to see what it connects to and where it comes from."});
    list.push({title:"2. What it is made of", view:"components", sel:handler ? handler.id : null, text:"Read the columns from left to right. That is the order in which a request moves. "+(handler ? handler.name+" is in the "+LAYER_NAME[handler.layer].toLowerCase()+" column. " : "")+"The small squares are its ports."});
  }
  var tf = aspect("request_flows").state === "present" && tourFlow();
  if (tf){
    var st = tf.flow.steps[tf.step-1], ex = tf.flow.exits.filter(function(x){ return x.after_step === tf.step; })[0];
    list.push({title:"3. A worked example: "+opLabel(tf.flow), view:"flows", flow:tf.flow.id, step:tf.step, text:"Step "+tf.step+" of "+tf.flow.steps.length+": "+st.text+(ex ? " If "+ex.when+", the call stops: "+ex.result+"." : "")+" Press Next or the right arrow key to go on to the store and the events."});
    var later = tf.flow.steps.filter(function(x){ return x.mode === "async"; });
    var answer = later.length ? "Steps "+later.map(function(x){ return x.n; }).join(", ")+": "+later.map(function(x){ return nameOf(x.element); }).join(", ")+". The dashed arrows and the word later mark them." : "None. No step of this flow is marked later.";
    list.push({title:"4. Check yourself", view:"flows", flow:tf.flow.id, step:1, question:"In this flow, which steps run later, after the call has its response?", answer:answer});
  }
  return list;
}
var STOPS = stops();
function openTour(i){ tour.i = i || 0; showStop(); }
function closeTour(){ if (tour.el) tour.el.remove(); tour.el = null; try { localStorage.setItem("stego-arch-tour", "seen"); } catch(_){} $("tourBtn").focus(); }
function showStop(){
  var st = STOPS[tour.i];
  if (st.view){ state.view = st.view; state.text = false; if (st.flow){ state.flow = st.flow; state.step = st.step || 1; } state.sel = st.sel || (st.flow ? byId[st.flow].steps[(st.step||1)-1].element : null); render(); }
  if (tour.el) tour.el.remove();
  var body;
  if (st.body) body = st.body();
  else if (st.question){
    var ans = h("p", {id:"tourAnswer", hidden:"hidden", text:"Answer: "+st.answer});
    body = h("div", {}, [h("p", {id:"tourQuestion", text:st.question}), h("button", {"class":"btn", type:"button", id:"tourReveal", text:"Show the answer", onclick:function(){ ans.removeAttribute("hidden"); }}), ans]);
  } else body = h("p", {text:st.text});
  tour.el = h("div", {"class":"tour", id:"tour", role:"dialog", "aria-modal":"false", "aria-labelledby":"tourTitle"}, [
    h("h4", {id:"tourTitle", text:st.title}), body,
    h("div", {"class":"row"}, [
      h("span", {"class":"prog", text:(tour.i+1)+" of "+STOPS.length}),
      h("span", {}, [
        h("button", {"class":"btn", type:"button", id:"tourSkip", text:"Skip", onclick:closeTour}), " ",
        tour.i > 0 ? h("button", {"class":"btn", type:"button", id:"tourBack", text:"Back", onclick:function(){ tour.i--; showStop(); }}) : null, " ",
        h("button", {"class":"btn", type:"button", id:"tourNext", text:tour.i < STOPS.length-1 ? "Next" : "Done", onclick:function(){ if (tour.i < STOPS.length-1){ tour.i++; showStop(); } else closeTour(); }})
      ])
    ])
  ]);
  document.body.appendChild(tour.el);
  $("tourNext").focus();
}
$("tourBtn").addEventListener("click", function(){ openTour(0); });

var firstHash = location.hash;
readHash(); lastNarrow = narrow(); render();
var seen = null; try { seen = localStorage.getItem("stego-arch-tour"); } catch(_){}
if (!seen && !firstHash){
  tour.el = h("div", {"class":"tour", id:"tourOffer", role:"dialog", "aria-modal":"false", "aria-labelledby":"offerT"}, [h("h4", {id:"offerT", text:"New to this service?"}), h("p", {text:"Take a short tour: five words, the big picture, and one request step by step."}),
    h("div", {"class":"row"}, [h("span"), h("span", {}, [h("button", {"class":"btn", type:"button", id:"tourNo", text:"No thanks", onclick:closeTour}), " ", h("button", {"class":"btn", type:"button", id:"tourStart", text:"Start the tour", onclick:function(){ tour.el.remove(); tour.el = null; openTour(0); }})])])]);
  document.body.appendChild(tour.el);
}
})();
