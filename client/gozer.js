// gozer client: applies render batches from the server and forwards DOM events.
(() => {
  const SVG = 'http://www.w3.org/2000/svg';
  const script = document.currentScript;
  const mount = document.getElementById('gozer-app');
  const overlay = document.getElementById('gozer-reconnect');
  const errorBar = document.getElementById('gozer-error');
  let circuit = script.dataset.circuit || '';
  let applied = 0;
  let ws = null;
  let retries = 0;
  let connected = false; // overlay only after a working connection drops; crawlers never get one
  let dead = false;
  let outbox = [];
  const nodes = new Map([[1, mount]]); // id -> Node, or [marker, ...nodes] for raw html
  const calls = new Map(); // pending JS -> Go calls
  let callSeq = 0;

  function send(m) {
    const s = JSON.stringify(m);
    if (ws && ws.readyState === 1) ws.send(s); else outbox.push(s);
  }

  function connect() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const q = new URLSearchParams({ c: circuit, s: applied, u: location.pathname + location.search });
    ws = new WebSocket(`${proto}//${location.host}/_gozer/ws?${q}`);
    ws.onopen = () => {
      retries = 0;
      connected = true;
      overlay.hidden = true;
      const pending = outbox;
      outbox = [];
      for (const s of pending) ws.send(s);
    };
    ws.onmessage = (e) => receive(JSON.parse(e.data));
    ws.onclose = (e) => {
      ws = null;
      if (dead) return;
      if (e.code === 4001) { location.reload(); return; }
      if (e.code === 4002) { fatal(); return; }
      overlay.hidden = !connected;
      setTimeout(connect, Math.min(500 * 2 ** retries++, 5000));
    };
  }

  function fatal() {
    dead = true;
    overlay.hidden = true;
    errorBar.hidden = false;
  }

  function receive(m) {
    switch (m.t) {
      case 'h': circuit = m.c; break;
      case 'b':
        if (m.n <= applied) return;
        if (m.n !== applied + 1) { location.reload(); return; }
        if (m.n === 1) mount.replaceChildren(); // drop prerendered markup
        for (const op of m.o) apply(op);
        applied = m.n;
        send({ t: 'a', s: m.n });
        break;
      case 'j': callJS(m); break;
      case 'r': {
        const p = calls.get(m.i);
        if (!p) return;
        calls.delete(m.i);
        if (m.e) p.reject(new Error(m.e)); else p.resolve(m.v);
        break;
      }
      case 'x': fatal(); break;
    }
  }

  const first = (id) => { const n = nodes.get(id); return Array.isArray(n) ? n[0] : n; };
  const all = (id) => { const n = nodes.get(id); return Array.isArray(n) ? n : [n]; };
  const inSvg = (el) => el.namespaceURI === SVG && el.localName !== 'foreignObject';

  function forget(n) {
    if (n.__gz) nodes.delete(n.__gz);
    for (const c of n.childNodes) forget(c);
  }

  function parseHTML(html) {
    const t = document.createElement('template');
    t.innerHTML = html;
    return [...t.content.childNodes];
  }

  function build(s, svg) {
    let n;
    if (s.t !== undefined) {
      svg = svg || s.t === 'svg';
      n = svg ? document.createElementNS(SVG, s.t) : document.createElement(s.t);
      if (s.a) for (const k in s.a) n.setAttribute(k, s.a[k]);
      const childSvg = svg && s.t !== 'foreignObject';
      if (s.c) for (const c of s.c) for (const x of build(c, childSvg)) n.appendChild(x);
      if (s.p) for (const k in s.p) setProp(n, k, s.p[k]);
      if (s.e) for (const k in s.e) listen(n, s.i, k, s.e[k]);
    } else if (s.x !== undefined) {
      n = document.createTextNode(s.x);
    } else if (s.r !== undefined) {
      const marker = document.createComment('r');
      marker.__gz = s.i;
      const list = [marker, ...parseHTML(s.r)];
      nodes.set(s.i, list);
      return list;
    } else {
      n = document.createComment('gz');
    }
    n.__gz = s.i;
    nodes.set(s.i, n);
    return [n];
  }

  function setProp(el, k, v) {
    if (k === 'checked') { el.checked = !!v; return; }
    v = v ?? '';
    if (el[k] !== v) el[k] = v;
  }

  function listen(el, id, name, flags) {
    const hs = el.__gzh || (el.__gzh = {});
    if (hs[name]) el.removeEventListener(name, hs[name]);
    delete hs[name];
    if (flags < 0) return;
    hs[name] = (e) => {
      if (flags & 1 || name === 'submit') e.preventDefault();
      if (flags & 2) e.stopPropagation();
      send({ t: 'e', i: id, n: name, d: eventData(e) });
    };
    el.addEventListener(name, hs[name]);
  }

  function eventData(e) {
    const t = e.currentTarget;
    const d = { type: e.type };
    if (t && 'value' in t && typeof t.value === 'string') d.value = t.value;
    if (t && (t.type === 'checkbox' || t.type === 'radio')) d.checked = t.checked;
    for (const k of ['key', 'code', 'repeat', 'ctrlKey', 'shiftKey', 'altKey', 'metaKey',
      'clientX', 'clientY', 'offsetX', 'offsetY', 'button', 'buttons', 'detail', 'deltaX', 'deltaY']) {
      if (k in e && e[k] !== undefined) d[k] = e[k];
    }
    if (e.type === 'submit' && t instanceof HTMLFormElement) {
      d.form = {};
      for (const [k, v] of new FormData(t)) if (typeof v === 'string') (d.form[k] ||= []).push(v);
    }
    return d;
  }

  function apply(op) {
    switch (op[0]) {
      case 0: { // insert parent, before, specs
        const parent = nodes.get(op[1]);
        const before = op[2] ? first(op[2]) : null;
        const svg = parent !== mount && inSvg(parent);
        for (const s of op[3]) for (const x of build(s, svg)) parent.insertBefore(x, before);
        break;
      }
      case 1: for (const n of all(op[1])) { forget(n); n.remove(); } nodes.delete(op[1]); break;
      case 2: { // move parent, id, before
        const parent = nodes.get(op[1]);
        const before = op[3] ? first(op[3]) : null;
        for (const n of all(op[2])) parent.insertBefore(n, before);
        break;
      }
      case 3: op[3] === null ? nodes.get(op[1]).removeAttribute(op[2]) : nodes.get(op[1]).setAttribute(op[2], op[3]); break;
      case 4: setProp(nodes.get(op[1]), op[2], op[3]); break;
      case 5: nodes.get(op[1]).data = op[2]; break;
      case 6: { // raw html
        const [marker, ...old] = nodes.get(op[1]);
        for (const n of old) n.remove();
        const fresh = parseHTML(op[2]);
        marker.after(...fresh);
        nodes.set(op[1], [marker, ...fresh]);
        break;
      }
      case 7: { const el = nodes.get(op[1]); listen(el, op[1], op[2], op[3]); break; }
      case 8: // navigate url, replace, force
        if (op[3]) { op[2] ? location.replace(op[1]) : location.assign(op[1]); dead = true; return; }
        history[op[2] ? 'replaceState' : 'pushState'](null, '', op[1]);
        window.scrollTo(0, 0);
        break;
      case 9: document.title = op[1]; break;
      case 10: // head html
        for (const n of document.head.querySelectorAll('[data-gz-head]')) n.remove();
        for (const n of parseHTML(op[1])) if (n.nodeType === 1) document.head.appendChild(n);
        break;
    }
  }

  function revive(v) {
    if (Array.isArray(v)) return v.map(revive);
    if (v && typeof v === 'object') {
      if ('__gzref' in v) return first(v.__gzref);
      for (const k in v) v[k] = revive(v[k]);
    }
    return v;
  }

  async function callJS(m) {
    try {
      const path = m.f.split('.');
      let self = window;
      for (let i = 0; i < path.length - 1; i++) self = self[path[i]];
      const fn = self[path[path.length - 1]];
      if (typeof fn !== 'function') throw new Error(`${m.f} is not a function`);
      let v = await fn.apply(self, revive(m.a));
      if (v === undefined || v instanceof Node) v = null;
      send({ t: 'r', i: m.i, v });
    } catch (err) {
      send({ t: 'r', i: m.i, e: String((err && err.message) || err) });
    }
  }

  document.addEventListener('click', (e) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const a = e.target.closest && e.target.closest('a[href]');
    if (!a || !mount.contains(a) || (a.target && a.target !== '_self') || a.hasAttribute('download') || a.hasAttribute('data-gozer-reload')) return;
    const u = new URL(a.href, location.href);
    if (u.origin !== location.origin || u.pathname.startsWith('/_gozer/')) return;
    if (u.pathname === location.pathname && u.search === location.search && u.hash) return;
    e.preventDefault();
    history.pushState(null, '', u);
    window.scrollTo(0, 0);
    send({ t: 'n', u: u.pathname + u.search });
  });
  window.addEventListener('popstate', () => send({ t: 'n', u: location.pathname + location.search }));

  window.gozer = {
    invoke(name, ...args) {
      const i = ++callSeq;
      return new Promise((resolve, reject) => {
        calls.set(i, { resolve, reject });
        send({ t: 'c', i, f: name, a: args });
      });
    },
    focus(el) { el.focus(); },
  };

  connect();
})();
