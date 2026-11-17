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
var GLYPH = {"direct":"●","arrow":"↑","userset":"◎","caveat":"?","intersection":"&","exclusion":"−","wildcard":"*","recursion":"↻","depth limit":"⋯","expiration":"⌛","work limit":"⊘","path limit":"⊞"};
var state = {view:"overview", sel:null, flow:null, step:1, text:false, coll:"", q:""};
// byId has no prototype, so a key that the model or the address gives never reads a property of Object (#342 F5).
var byId = Object.create(null);

/* ---------- index of every element of the model ---------- */
function put(list){ (list||[]).forEach(function(e){ if (e && e.id) byId[e.id] = e; }); }
[M.containers,M.actors,M.relations,M.components,M.mixins,M.ports,M.bindings,M.slots,M.fills,M.entities,M.references,M.collections,M.operations,
 M.actions,M.flows,M.needs,M.boundaries,M.settings,M.authn_points,M.authz_points,M.access.checks,M.access.permissions,M.access.roles].forEach(put);
M.entities.forEach(function(e){ put(e.fields); });
M.access.permissions.forEach(function(p){ put(p.paths); });
// PERM gives each permission of the model by its ID, so that a grant that names another element is no permission, as
// in the Go view (security review of PR #388, Info).
var PERM = Object.create(null);
M.access.permissions.forEach(function(p){ if (p && p.id) PERM[p.id] = p; });
if (M.flows.length) state.flow = M.flows[0].id;

function short(id){ var i = String(id).indexOf(":"); return i >= 0 ? String(id).slice(i+1) : String(id); }
function kindOf(id){ var i = String(id).indexOf(":"); return i >= 0 ? String(id).slice(0,i) : ""; }
function nameOf(id){ var e = byId[id]; if (!e) return short(id); if (kindOf(id) === "relation") return nameOf(e.from)+" "+e.text+" "+nameOf(e.to); if (kindOf(id) === "slot") return short(e.collection)+"."+e.name; if (kindOf(id) === "operation" || kindOf(id) === "flow") return short(id); return e.name || e.label || e.path || short(id); }
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
  M.access.checks.forEach(function(c){ if (c.subject === id) push("checks", c.id); if (c.id === id){ push("subject", c.subject); push("grants", c.grants); } if (c.grants === id) push("checked by", c.subject);
    // The parent chain check: each ancestor that the operation reads first, its link field and its permission (ST-SUP-RC20-OC24).
    (c.ancestors || []).forEach(function(a){ [a.entity, a.link, a.grants].forEach(function(x){ if (!x) return; if (c.id === id) push("reads ancestor", x); if (x === id) push("read as an ancestor by", c.id); }); }); });
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
// label writes a text that may be cut short; a cut text keeps its full form as its title, which shows on hover, and
// the element that holds it names it in full for focus (#hoverInfo).
function label(attrs, text, max){ var t = s("text", attrs); t.textContent = clip(text, max); if (String(text).length > max) t.appendChild(s("title", {text:String(text)})); return t; }
function node(id, p, name, sub, color, extra){
  var full = name+(sub ? ", "+sub : "")+(extra ? ", "+extra : "");
  var g = s("g", {"class":"el", tabindex:"0", role:"button", "data-id":id, "aria-label":full});
  var x = p.x, y = p.y, w = p.w, ht = p.h, box, st = "fill:color-mix(in srgb, "+color+" 16%, var(--panel));stroke:"+color+";stroke-width:2";
  if (p.shape === "store") box = s("path", {"class":"box", d:"M"+x+" "+(y+8)+" a"+(w/2)+" 8 0 0 0 "+w+" 0 v"+(ht-16)+" a"+(w/2)+" 8 0 0 1 -"+w+" 0 z M"+x+" "+(y+8)+" a"+(w/2)+" 8 0 0 1 "+w+" 0", style:st});
  else if (p.shape === "slot"){ var c = Math.min(12, ht/2.5); box = s("path", {"class":"box", d:"M"+(x+c)+" "+y+" H"+(x+w-c)+" L"+(x+w)+" "+(y+ht/2)+" L"+(x+w-c)+" "+(y+ht)+" H"+(x+c)+" L"+x+" "+(y+ht/2)+" Z", style:st}); }
  else if (p.shape === "entry") box = s("rect", {"class":"box", x:x, y:y, width:w, height:ht, rx:ht/2, style:st+";stroke-dasharray:5 3"});
  else if (p.shape === "gate") box = s("rect", {"class":"box", x:x, y:y, width:w, height:ht, rx:4, style:"fill:var(--panel);stroke:var(--ink);stroke-width:2.5"});
  else if (p.shape === "chip") box = s("rect", {"class":"box", x:x, y:y, width:w, height:ht, rx:ht/2, style:st});
  else box = s("rect", {"class":"box", x:x, y:y, width:w, height:ht, rx:p.shape === "system" ? 2 : 8, style:st+(p.shape === "system" ? ";stroke-width:3;paint-order:stroke" : "")});
  g.appendChild(box);
  if (p.shape === "system") g.appendChild(s("rect", {x:x+4, y:y+4, width:w-8, height:ht-8, rx:1, style:"fill:none;stroke:"+color+";stroke-width:1"}));
  var pad = 0;
  if (p.shape === "actor"){
    // A person glyph: the caller and the other actors that act on the service.
    var gx = x+16, gy = y+ht/2;
    g.appendChild(s("circle", {cx:gx, cy:gy-8, r:5, style:"fill:none;stroke:var(--ink);stroke-width:1.6"}));
    g.appendChild(s("path", {d:"M"+(gx-9)+" "+(gy+10)+" q9 -14 18 0", style:"fill:none;stroke:var(--ink);stroke-width:1.6"}));
    pad = 14;
  }
  var text = (p.shape === "gate" ? "→ " : "")+name;
  var max = Math.max(4, Math.floor((w-pad-10)/6.6)), cx = x+pad+(w-pad)/2, ty = y + (sub ? ht/2 - 3 : ht/2 + 4) + (p.shape === "store" ? 5 : 0);
  g.appendChild(label({x:cx, y:ty, "text-anchor":"middle", "font-weight":"600"}, text, max));
  if (sub) g.appendChild(label({"class":"sub", x:cx, y:ty+14, "text-anchor":"middle"}, sub, max+2));
  g.appendChild(s("title", {text:full}));
  return g;
}
function line(svg, x1, y1, x2, y2, label, o){
  o = o || {};
  var mx = (x1+x2)/2, my = (y1+y2)/2;
  var d = o.vertical ? "M"+x1+" "+y1+" C"+x1+" "+my+" "+x2+" "+my+" "+x2+" "+y2 : "M"+x1+" "+y1+" C"+mx+" "+y1+" "+mx+" "+y2+" "+x2+" "+y2;
  svg.appendChild(s("path", {"class":"edge-line"+(o.async ? " async" : ""), d:d, "marker-end":"url(#arr)", "data-a":o.a||"", "data-b":o.b||"", "data-rel":o.rel||""}));
  if (label){ var lx = o.at === "end" ? x2-8 : o.at === "start" ? x1+8 : mx+(o.dx||0), ly = o.at === "end" ? y2-6 : o.at === "start" ? y1-5 : my-5;
    svg.appendChild(s("text", {"class":"sub edge-label", x:lx, y:ly, "text-anchor":o.at === "end" ? "end" : o.at === "start" ? "start" : "middle", text:label})); }
}
function connect(svg, a, b, label, o){
  o = o || {};
  if (a.x === b.x || o.vertical){ var down = b.y > a.y; line(svg, a.x+a.w/2, down ? a.y+a.h : a.y, b.x+b.w/2, down ? b.y : b.y+b.h, label, {vertical:true, async:o.async, a:o.a, b:o.b, dx:o.dx}); }
  else if (b.x > a.x) line(svg, a.x+a.w, a.y+a.h/2, b.x, b.y+b.h/2, label, o);
  else line(svg, a.x, a.y+a.h/2, b.x+b.w, b.y+b.h/2, label, o);
}
function posMap(d){ var m = Object.create(null); d.nodes.forEach(function(n){ m[n.id] = n; }); return m; }
function emptyCard(reason){ return h("div", {"class":"empty", role:"note"}, [h("strong", {text:"This aspect has no element yet."}), h("p", {text:reason})]); }
function form(view){ return $("form-"+view); }
function cloneForm(view, selector){
  var f = form(view), box = h("div");
  if (!f) return box;
  Array.prototype.forEach.call(f.children, function(c){ if (c.tagName !== "H2" && !(c.classList && c.classList.contains("question"))) box.appendChild(c.cloneNode(true)); });
  if (selector){ var only = h("div"); Array.prototype.forEach.call(box.querySelectorAll(selector), function(x){ only.appendChild(x); }); return only; }
  return box;
}
// swatch draws the mark of one item of a key, in the same shape, line and colour as the diagram draws it.
function swatch(kind, arg){
  var sv = s("svg", {width:"30", height:"16", viewBox:"0 0 30 16", "aria-hidden":"true", "class":"swatch"}), c = arg && arg.indexOf(" ") < 0 ? col(arg) : "var(--line)";
  var st = "fill:color-mix(in srgb, "+c+" 16%, var(--panel));stroke:"+c+";stroke-width:2";
  if (kind === "box") sv.appendChild(s("rect", {x:3, y:2, width:24, height:12, rx:3, style:st}));
  else if (kind === "system"){ sv.appendChild(s("rect", {x:3, y:2, width:24, height:12, rx:1, style:st})); sv.appendChild(s("rect", {x:5.5, y:4.5, width:19, height:7, style:"fill:none;stroke:"+c+";stroke-width:1"})); }
  else if (kind === "entry") sv.appendChild(s("rect", {x:2, y:3, width:26, height:10, rx:5, style:st+";stroke-dasharray:4 2"}));
  else if (kind === "chip") sv.appendChild(s("rect", {x:2, y:3, width:26, height:10, rx:5, style:st}));
  else if (kind === "gate") sv.appendChild(s("rect", {x:2, y:2, width:26, height:12, rx:2, style:"fill:var(--panel);stroke:var(--ink);stroke-width:2.5"}));
  else if (kind === "hex") sv.appendChild(s("path", {d:"M7 2 H23 L28 8 L23 14 H7 L2 8 Z", style:st}));
  else if (kind === "store") sv.appendChild(s("path", {d:"M4 4 a11 2.5 0 0 0 22 0 v8 a11 2.5 0 0 1 -22 0 z M4 4 a11 2.5 0 0 1 22 0", style:st}));
  else if (kind === "actor"){ sv.appendChild(s("circle", {cx:15, cy:5, r:3.5, style:"fill:none;stroke:var(--ink);stroke-width:1.5"})); sv.appendChild(s("path", {d:"M8 15 q7 -10 14 0", style:"fill:none;stroke:var(--ink);stroke-width:1.5"})); }
  else if (kind === "dash" || kind === "hatch") sv.appendChild(s("rect", {x:2, y:2, width:26, height:12, rx:3, style:"fill:"+(kind === "hatch" ? "url(#hatch-key)" : "none")+";stroke:var(--ink);stroke-width:2;stroke-dasharray:"+(arg || "none")}));
  else if (kind === "line"){ sv.appendChild(s("path", {d:"M2 8 H24", style:"stroke:var(--line);stroke-width:2;stroke-dasharray:"+(arg || "none")})); sv.appendChild(s("path", {d:"M22 4 L28 8 L22 12 z", style:"fill:var(--line)"})); }
  else if (kind === "dot"){ sv.appendChild(s("circle", {cx:15, cy:8, r:6, style:"fill:var(--panel);stroke:"+(arg ? col(arg) : "var(--ink)")+";stroke-width:2"})); }
  else return null;
  return sv;
}
function setKey(items){
  var k = $("key"); k.textContent = "";
  items.forEach(function(it){ k.appendChild(h("li", {}, [swatch(it[0], it[1]), h("span", {text:it[2]})])); });
}
function layerKey(){ M.layers.forEach(function(l){ $("key").appendChild(h("li", {}, [swatch("box", l), h("span", {text:"column "+(M.layers.indexOf(l)+1)+": "+LAYER_NAME[l]})])); }); }

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
  if (k === "actor") return e.kind;
  return "";
}
function labelOf(id){ var e = byId[id]; return kindOf(id) === "slot" ? short(e.collection) : nameOf(id); }

function renderOverview(root){
  var d = pick(L.overview), P = posMap(d), n = narrow(), svg = svgRoot(d.w, d.h, "Context diagram of "+SYS);
  (d.regions||[]).forEach(function(r){ r.rects.forEach(function(rc){
    svg.appendChild(s("rect", {x:rc[0], y:rc[1], width:rc[2], height:rc[3], rx:14, style:"fill:none;stroke:var(--ink);stroke-width:2;stroke-dasharray:10 5"}));
    svg.appendChild(s("rect", {x:rc[0]+8, y:rc[1]-9, width:Math.min(rc[2]-16, 8+7.2*byId[r.id].label.length), height:18, style:"fill:var(--panel)"}));
    svg.appendChild(s("text", {x:rc[0]+12, y:rc[1]+4, "font-weight":"700", text:byId[r.id].label}));
  }); });
  (d.edges||[]).forEach(function(e){ var a = P[e.from], b = P[e.to]; if (!a || !b) return;
    var right = kindOf(e.from) === "container";
    connect(svg, a, b, right || n ? "" : e.label, {a:e.from, b:e.to, rel:e.relation, async:e.async, vertical:n, dx:n ? 60 : 0, at:n ? "" : "start"}); });
  d.nodes.forEach(function(p){
    var e = byId[p.id], sub = subOf(p.id), color = p.shape === "entry" ? col("edge") : col(p.layer);
    if (p.shape === "entry" && n){ var cnt = (d.edges||[]).filter(function(x){ return x.from === p.id && x.label; })[0]; if (cnt) sub = cnt.label+" · "+short(e.entity); }
    if (kindOf(p.id) === "actor" || (kindOf(p.id) === "container" && e && e.kind !== "service")){
      var rel = M.relations.filter(function(r){ return r.to === p.id; })[0];
      // The relation names the target in its second line, so no label crosses the lines that come in.
      if (rel) sub = rel.text+(rel.through ? " · "+short(rel.through) : "");
    }
    svg.appendChild(node(p.id, p, labelOf(p.id), sub, color));
  });
  root.appendChild(h("div", {"class":"canvas"}, [svg]));
  setKey([["actor","edge","Person: the caller"],["entry","edge","Dashed pill: a collection that callers reach through the network edge"],["box","handler","Box: the service process"],
    ["system","outside","Double box: a system outside the service"],["store","store","Cylinder: a store"],["dash","10 5","Long dashes: the network edge"],["line","","Solid arrow: a call in the request"],["line","6 4","Dashed arrow: work that runs later"]]);
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
    if (!d.nodes.some(function(n){ return n.layer === l.layer; })) svg.appendChild(s("text", {"class":"sub", x:l.x+8, y:l.y+34, text:"no component"}));
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

// stepLane gives the index of the layer of a step, which the model gives (steps[].layer).
function stepLane(f, i){ return Math.max(0, M.layers.indexOf(f.steps[i].layer)); }
function openFlow(id, step){ if (!id || !byId[id]) return; state.view = "flows"; state.flow = id; state.step = step || 1; state.sel = byId[id].steps[(step||1)-1].element; render(); }
function opLabel(f){ var o = byId[f.operation]; return o ? (o.method ? o.method+" " : "")+short(o.collection)+" · "+o.verb : short(f.id); }
function renderFlows(root){
  var mini = h("div", {"class":"mini", role:"group", "aria-label":"All request flows, one for each operation"});
  M.flows.forEach(function(f){
    var b = h("button", {type:"button", "aria-pressed":String(f.id === state.flow), "data-flow":f.id, "aria-label":opLabel(f)+", "+f.steps.length+" steps, "+f.exits.length+" exits", onclick:function(){ openFlow(f.id, 1); }});
    b.appendChild(h("span", {"class":"t", text:opLabel(f)}));
    // A small lane chart: each step is a dot, its height the layer (edge at the top), so the layer shows by place
    // and not by colour alone; a hollow dot runs later.
    var sv = s("svg", {viewBox:"0 0 160 "+(M.layers.length*5+6), "aria-hidden":"true", "class":"lanes"});
    var pts = f.steps.map(function(st, i){ return [6+i*12, 4+stepLane(f, i)*5]; });
    sv.appendChild(s("path", {d:"M"+pts.map(function(q){ return q[0]+" "+q[1]; }).join(" L"), style:"fill:none;stroke:var(--line);stroke-width:1"}));
    f.steps.forEach(function(st, i){ var c = col(M.layers[stepLane(f, i)]); sv.appendChild(s("circle", {cx:pts[i][0], cy:pts[i][1], r:3.6, style:"fill:"+(st.mode === "async" ? "var(--panel)" : c)+";stroke:"+c+";stroke-width:1.6"})); });
    b.appendChild(sv);
    b.appendChild(h("span", {"class":"k", text:f.steps.length+" steps · "+f.exits.length+(f.exits.length === 1 ? " exit" : " exits")}));
    mini.appendChild(b);
  });
  root.appendChild(h("h3", {text:"All flows"}));
  root.appendChild(mini);
  if (M.actions.length) root.appendChild(h("p", {"class":"question", id:"noFlowActions", text:"The model has a request flow only for each operation of a collection. These commands, queries and workers have no flow here; the access matrix shows what each one checks: "+M.actions.map(function(a){ return a.name+" ("+a.kind+")"; }).join(", ")+"."}));
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
    // The lanes, and room on the right for the names and the exits of the steps in the last lanes.
    var lw = L.flows.lane_w, W = L.flows.w+190, rh = 44, H = n*rh+44, svg = svgRoot(W, H, "Lane of each step of "+opLabel(f));
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
      // The name of the step's element beside its dot, so the diagram reads without the list.
      var nm = label({"class":cur ? "step-name cur" : "step-name", x:x+16, y:y-1, "data-step":String(i+1)}, nameOf(st.element), Math.max(10, Math.floor((W-x-20)/6.4)));
      nm.addEventListener("click", function(){ state.step = i+1; state.sel = st.element; render(); });
      svg.appendChild(nm);
      f.exits.filter(function(e){ return e.after_step === i+1; }).forEach(function(e, k){
        svg.appendChild(s("path", {"class":"edge-line", d:"M"+(x+10)+" "+(y+6)+" l22 "+(6+k*12), style:"stroke-dasharray:2 2", "marker-end":"url(#arr)"}));
        svg.appendChild(label({"class":"sub", x:x+36, y:y+16+k*12}, "↳ "+e.result, Math.max(14, Math.floor((W-x-40)/6))));
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
    if (st.kind === "store" && byId[st.element] && byId[st.element].layer && byId[st.element].layer !== "store") btn.appendChild(h("span", {"class":"exit", "data-note":"store", text:"No component of the active set is in the store layer, so the model names "+nameOf(st.element)+", the "+LAYER_NAME[byId[st.element].layer].toLowerCase()+" that writes the store."}));
    f.exits.filter(function(e){ return e.after_step === i+1; }).forEach(function(e){ btn.appendChild(h("span", {"class":"exit", text:"↳ stops: "+e.result+", when "+e.when})); });
    li.appendChild(btn); ol.appendChild(li);
  });
  root.appendChild(ol);
  var st = f.steps[state.step-1];
  if (!state.sel) state.sel = st.element;
  $("live").textContent = "Step "+state.step+" of "+n+": "+nameOf(st.element)+". "+st.text;
  setKey([["dot","","Dot: a step, in the column of its layer; a filled dot is done"],["line","","Solid arrow: in the request"],["line","6 4","Dashed arrow: later, after the response"],["line","2 2","↳ stops: where the call can stop, and its result"]]);
}
function go(dir){
  var f = byId[state.flow]; if (!f) return;
  var nx = state.step+dir; if (nx < 1 || nx > f.steps.length) return;
  state.step = nx; state.sel = f.steps[nx-1].element; render(); focusStep();
}
function focusStep(){ var b = document.querySelector(".steps li.cur button"); if (b) b.focus(); }

// formSections gives the parts of the text form of a view by the title of each part: the title and what follows it.
function formSections(view){
  var out = {}, f = cloneForm(view);
  Array.prototype.forEach.call(f.querySelectorAll("h3"), function(hh){
    var part = [hh], n = hh.nextElementSibling;
    while (n && n.tagName !== "H3"){ part.push(n); n = n.nextElementSibling; }
    out[hh.textContent] = part;
  });
  return out;
}
// fold puts a part of a text form behind a disclosure control, closed: details on demand.
function fold(part, id){
  if (!part) return null;
  var title = part[0].textContent, rows = 0;
  part.slice(1).forEach(function(x){ rows += x.querySelectorAll ? x.querySelectorAll("tbody tr, li").length : 0; });
  var d = h("details", {"class":"more", id:id}, [h("summary", {}, [h("span", {"class":"t", text:title}), h("span", {"class":"k", text:rows ? rows+" lines" : ""})])]);
  part.slice(1).forEach(function(x){ d.appendChild(x); });
  return d;
}
function renderTrust(root){
  var d = narrow() ? L.trust.narrow : L.trust.wide, svg = svgRoot(d.w, d.h, "Trust boundaries of "+SYS+": each boundary, what it holds, and what crosses it");
  d.regions.slice().sort(function(a, b){ return a.depth-b.depth; }).forEach(function(r){
    var b = byId[r.id]; if (!b) return;
    var dash = BOUNDARY_STYLE[b.kind] || "", lw = Math.min(r.w-16, 14+7.6*b.kind.length);
    var g = s("g", {"class":"el region", tabindex:"0", role:"button", "data-id":r.id, "aria-label":"Boundary "+b.kind+": "+b.label+". It holds "+b.contains.length+" and "+b.crossed_by.length+" cross it."});
    g.appendChild(s("rect", {"class":"box", x:r.x, y:r.y, width:r.w, height:r.h, rx:12, style:"fill:"+(b.kind === "fill code" ? "url(#hatch)" : "color-mix(in srgb, var(--ink) "+(2+r.depth*2)+"%, var(--panel))")+";stroke:var(--ink);stroke-width:2;stroke-dasharray:"+(dash || "none")}));
    g.appendChild(s("rect", {x:r.x+10, y:r.y-10, width:lw, height:20, rx:4, style:"fill:var(--panel)"}));
    g.appendChild(s("text", {x:r.x+17, y:r.y+5, "font-weight":"700", text:b.kind}));
    g.appendChild(s("title", {text:b.kind+": "+b.label}));
    svg.appendChild(g);
  });
  d.nodes.forEach(function(p){
    var color = p.shape === "actor" ? col("edge") : col(p.layer || "outside call"), extra = "";
    if (p.shape === "gate"){
      var crossed = M.boundaries.filter(function(b){ return b.crossed_by.indexOf(p.id) >= 0 || (kindOf(p.id) === "collection" && b.kind === "network"); })[0];
      extra = crossed ? "crosses the "+crossed.kind : "";
    }
    var sub = "";
    if (kindOf(p.id) === "collection") extra = (byId[p.id] ? byId[p.id].operations.length+" operations, " : "")+extra;
    svg.appendChild(node(p.id, p, nameOf(p.id), sub, color, extra));
  });
  root.appendChild(h("div", {"class":"canvas trustmap"}, [svg]));
  var key = h("ul", {"class":"key", "aria-label":"Key of the boundary map"});
  [["dash","10 5","network edge"],["dash","","verified identity"],["dash","3 3","tenant scope"],["hatch","","fill code (owner code)"],["dash","1 4","process that runs later"],
   ["gate","","→ on a border: it crosses that boundary"],["chip","handler","inside: the boundary holds it"],["hex","slot","a slot"],["actor","","the caller and the token issuer, outside"]].forEach(function(it){ key.appendChild(h("li", {}, [swatch(it[0], it[1]), h("span", {text:it[2]})])); });
  root.appendChild(key);
  var parts = formSections("trust");
  var matrix = parts["Access matrix: who may call each operation"];
  if (matrix){
    root.appendChild(h("h3", {text:matrix[0].textContent}));
    root.appendChild(h("p", {"class":"question", text:"Select a cell to see who gets the permission and through which path, what the operation checks, and where it is decided."}));
    matrix.slice(1).forEach(function(x){ root.appendChild(x); });
  }
  var tenancy = parts["Tenancy"];
  if (tenancy){ root.appendChild(h("h3", {text:"Tenancy"})); tenancy.slice(1).forEach(function(x){ root.appendChild(x); }); }
  root.appendChild(h("h3", {text:"More detail"}));
  [["Where a caller is authenticated, and where authorization is decided","moreAuth"],["What each operation checks","moreChecks"],["Grant paths","moreGrants"],["Trust boundaries","moreBoundaries"]].forEach(function(x){
    var f = fold(parts[x[0]], x[1]); if (f) root.appendChild(f);
  });
  Array.prototype.forEach.call(root.querySelectorAll("button.cell"), function(b){
    var id = "cell:"+b.getAttribute("data-check")+"|"+b.getAttribute("data-role");
    b.setAttribute("aria-pressed", String(state.sel === id));
    b.addEventListener("click", function(ev){ ev.stopPropagation(); select(id); });
  });
  Array.prototype.forEach.call(root.querySelectorAll(".access th[data-id]"), function(th){
    var id = th.getAttribute("data-id"); if (!byId[id]) return;
    var b = h("button", {"class":"linkish head", type:"button", "aria-label":"Details of "+th.textContent, onclick:function(ev){ ev.stopPropagation(); select(id); }});
    while (th.firstChild) b.appendChild(th.firstChild);
    th.appendChild(b);
  });
  setKey([]);
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
  var parts = id.slice(5).split("|"), c = byId[parts[0]], role = parts[1], p = c && PERM[c.grants];
  d.appendChild(h("span", {"class":"type", text:"access matrix cell"}));
  d.appendChild(h("h3", {text:(role ? short(role) : "Paths that stop")+" · "+short(c.subject)}));
  var dl = h("dl");
  var point = (M.authz_points||[]).filter(function(x){ return x.element === c.subject; })[0];
  [["Operation", nameOf(c.subject)], ["Checks", c.public ? "public" : c.resource_type+"#"+c.permission], ["Decided", point ? point.text : ""], ["Source", srcText(c.source)]].forEach(function(r){ if (!r[1]) return; dl.appendChild(h("dt", {text:r[0]})); dl.appendChild(h("dd", {text:r[1]})); });
  d.appendChild(dl);
  if (c.ancestors && c.ancestors.length){
    // The parent chain check (ST-SUP-RC20-OC24): each ancestor must exist and be of the ancestor above it; only the
    // parent of a list, a create or an upsert is decided (ST-SUP-Q156).
    d.appendChild(h("strong", {text:"Ancestors in the path, the top one first"}));
    var al = h("ol", {id:"cellAncestors"}), cut = [], gone = [];
    c.ancestors.forEach(function(a){
      // The grants of the ancestor read are looked up among the permissions alone, as for the check (security review of
      // PR #390, L1). A grant that names no permission, in a model that the reader edited, is not held by a named path.
      var q = a.grants ? PERM[a.grants] : undefined, held = !!(q && Array.isArray(q.paths) && q.paths.some(function(g){ return (g.role || "") === role; }));
      // An ancestor read that states a permission whose grants the model does not hold may be held by any role, so the
      // cell never shows it as a definite no (security review of PR #390, L1).
      var names = a.decided === true && !a.public && !!(a.permission || a.grants), unknown = names && !q;
      // A limit cut the grant paths of the ancestor read short: more roles may hold it than the paths name, so the cell
      // never shows that set as complete (security review of PR #374, L2). A permission that does not say it is
      // complete counts as cut short, as in the Go view (security review of PR #383, L3).
      var partial = names && (!!(q && q.complete !== true) || unknown);
      var what = a.decided !== true ? "exists and linked"+(a.reason ? ": "+a.reason : "") : a.permission ? a.resource_type+"#"+a.permission : (a.public ? "public"+(a.reason ? ": "+a.reason : "") : "no permission: "+(a.reason || ""));
      var who = !names || !role ? "" : (held ? "; this role grants it" : (unknown ? "; the model holds no grant path of it, so this role may hold it" : (partial ? "; no path that the model names gives it to this role alone, but a limit cut its paths short, so this role may hold it" : "; this role alone does not grant it, so the caller also needs it")));
      var by = a.scope_source ? srcText(a.scope_source) : "";
      if (unknown) gone.push(nameOf(a.entity)+" ("+what+")");
      else if (partial) cut.push(nameOf(a.entity)+" ("+what+")");
      al.appendChild(h("li", {"data-ancestor":a.id}, [nameOf(a.entity)+" ("+a.param+"): "+what+(unknown ? " ⚠ incomplete: the model holds no grant path of it" : (partial ? " ⚠ incomplete: a limit was reached" : ""))+who+(by ? "; in the path by "+by : "")]));
    });
    d.appendChild(al);
    if (cut.length) d.appendChild(h("p", {"class":"incomplete", role:"note", id:"cellAncestorIncomplete", text:"⚠ Incomplete: a limit was reached in the grant paths of the ancestor read "+cut.join(", ")+", so it may grant more roles than its paths name."}));
    if (gone.length) d.appendChild(h("p", {"class":"incomplete", role:"note", id:"cellAncestorUnknown", text:"⚠ Incomplete: the model holds no grant path of the ancestor read "+gone.join(", ")+", so it may grant roles that the model does not name."}));
    d.appendChild(h("p", {"class":"k", text:"Each ancestor must exist in the tenant and be of the ancestor above it. A missing, denied or wrong ancestor answers 404, and the row is not written."}));
  }
  var own = (p && Array.isArray(p.paths) ? p.paths : []).filter(function(g){ return (g.role || "") === role; });
  if (!c.public && !p){
    // The model holds no permission of the name that the check grants by, as in a model that the reader edited: a role
    // may hold it, and the cell never reads as no grant (security review of PR #388, Info).
    d.appendChild(h("p", {"class":"incomplete", role:"note", id:"cellIncomplete", text:"⚠ Incomplete: the model holds no grant path of "+c.resource_type+"#"+c.permission+", so it may grant roles that the model does not name."}));
    d.appendChild(h("p", {"class":"incomplete", role:"note", id:"cellMayHold", text:"⚠ May hold: the model holds no grant path of the permission "+c.resource_type+"#"+c.permission+", so "+(role ? short(role) : "a role")+" may hold it."}));
  } else if (!c.public && p.complete !== true){
    d.appendChild(h("p", {"class":"incomplete", role:"note", id:"cellIncomplete", text:"⚠ Incomplete: a limit was reached, so "+c.resource_type+"#"+c.permission+" may grant more roles than its paths name."}));
    // No named path gives the role the permission, but a limit cut its paths short: the role may hold it, and the
    // cell never reads as a definite no (security review of PR #383, L1).
    if (role && !own.length) d.appendChild(h("p", {"class":"incomplete", role:"note", id:"cellMayHold", text:"⚠ May hold: no path that the model names gives "+short(role)+" the permission "+c.resource_type+"#"+c.permission+", but a limit cut its grant paths short, so this role may hold it."}));
    // The line of the paths that stop, when the model names no path of the permission that stops: a role may hold it
    // past the limit, and the cell never reads as no grant (security review of PR #385, L2).
    else if (!own.length) d.appendChild(h("p", {"class":"incomplete", role:"note", id:"cellMayHold", text:"⚠ May hold: the model names no path of the permission "+c.resource_type+"#"+c.permission+" that stops, but a limit cut its grant paths short, so a role may hold it by a path past the limit."}));
  }
  d.appendChild(h("strong", {text:"Grant paths"}));
  var ul = h("ul");
  own.forEach(function(g){
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
  if (!e){ d.classList.add("closed"); return; }
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
    if (v.id !== "platform") tb.appendChild(h("span", {"class":"k hint", text:"Select any box, row, step or cell for its details."}));
    if (state.text){ root.appendChild(cloneForm(v.id)); }
    else v.render(root);
  }
  wire(); paintSel(); details(); writeHash(); hoverInfo(); placeTour();
}
// hoverInfo gives each diagram a line under it that names in full the element under the pointer or the focus, so a
// label that the diagram cuts short has its full form on hover and on focus.
function hoverInfo(){
  Array.prototype.forEach.call(document.querySelectorAll("#view .canvas"), function(c){
    var svg = c.querySelector("svg"); if (!svg) return;
    var vb = svg.viewBox.baseVal; if (vb && vb.width > 900) svg.style.setProperty("--min-w", Math.round(vb.width*0.8)+"px");
    var info = h("p", {"class":"hover-info", "aria-hidden":"true"});
    c.appendChild(info);
    var show = function(ev){ var g = ev.target.closest && ev.target.closest(".el"); info.textContent = g ? g.getAttribute("aria-label") : ""; };
    svg.addEventListener("mouseover", show); svg.addEventListener("focusin", show);
  });
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
// placeTour puts the tour card beside the view: in the side column on a wide screen, or on a narrow one in the flow
// of the view, just before the part that the stop explains, scrolled so that the card and that part show together.
function placeTour(){
  if (!tour.el) return;
  if (!narrowScreen()){ if (tour.el.parentNode !== $("tourSlot")) $("tourSlot").appendChild(tour.el); return; }
  var at = $("flowTitle") || document.querySelector("#view .canvas") || $("view").firstChild;
  if (at && at.parentNode) at.parentNode.insertBefore(tour.el, at); else $("tourInline").appendChild(tour.el);
}
function narrowScreen(){ return !(window.matchMedia && window.matchMedia("(min-width: 901px)").matches); }
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
  placeTour();
  // The card and the part of the view that it explains show together: on a narrow screen the card stands just
  // before that part; on a wide one the card stays in the side column, and the view scrolls to that part.
  if (narrowScreen()) tour.el.scrollIntoView({block:"start"});
  else if (st.flow && $("flowTitle")) $("flowTitle").scrollIntoView({block:"start"});
  else window.scrollTo(0, 0);
  $("tourNext").focus({preventScroll:narrowScreen()});
}
$("tourBtn").addEventListener("click", function(){ openTour(0); });

var firstHash = location.hash;
readHash(); lastNarrow = narrow(); render();
var seen = null; try { seen = localStorage.getItem("stego-arch-tour"); } catch(_){}
if (!seen && !firstHash){
  tour.el = h("div", {"class":"tour", id:"tourOffer", role:"dialog", "aria-modal":"false", "aria-labelledby":"offerT"}, [h("h4", {id:"offerT", text:"New to this service?"}), h("p", {text:"Take a short tour: five words, the big picture, and one request step by step."}),
    h("div", {"class":"row"}, [h("span"), h("span", {}, [h("button", {"class":"btn", type:"button", id:"tourNo", text:"No thanks", onclick:closeTour}), " ", h("button", {"class":"btn", type:"button", id:"tourStart", text:"Start the tour", onclick:function(){ tour.el.remove(); tour.el = null; openTour(0); }})])])]);
  placeTour();
}
})();
