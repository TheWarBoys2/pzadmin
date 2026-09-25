// A deliberately small DOM stand-in: just enough of the API that web/app.js
// uses, so the modal stack and dialog flows can be exercised under node.
// This is a test harness, not a browser.

class ClassList {
  constructor(node) { this.node = node; this.set = new Set(); }
  add(...names) { for (const n of names) if (n) this.set.add(n); this._sync(); }
  remove(...names) { for (const n of names) this.set.delete(n); this._sync(); }
  contains(n) { return this.set.has(n); }
  toggle(n, force) {
    const want = force === undefined ? !this.set.has(n) : force;
    if (want) this.set.add(n); else this.set.delete(n);
    this._sync();
    return want;
  }
  _sync() { this.node._class = Array.from(this.set).join(' '); }
}

class Node {
  constructor(tag) {
    this.tagName = String(tag || '').toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.attributes = {};
    this.dataset = {};
    this.style = {};
    this.listeners = {};
    this.nodeType = 1;
    this._text = '';
    this._class = '';
    this.classList = new ClassList(this);
    this.focused = false;
    this.disabled = false;
    this.value = '';
  }

  get className() { return this._class; }
  set className(v) {
    this._class = v || '';
    this.classList.set = new Set(String(v || '').split(/\s+/).filter(Boolean));
  }

  get textContent() {
    if (this.nodeType === 3) return this._text;
    return this.children.map((c) => c.textContent).join('');
  }
  set textContent(v) {
    if (this.nodeType === 3) { this._text = String(v); return; }
    this.children = [];
    this._text = '';
    if (String(v) !== '') this.append(textNode(String(v)));
  }

  get firstChild() { return this.children[0] || null; }
  get isConnected() {
    let n = this;
    while (n.parentNode) n = n.parentNode;
    return n === document || n._isRoot === true;
  }

  append(...kids) {
    for (const kid of kids) {
      if (kid === null || kid === undefined) continue;
      const node = kid.nodeType ? kid : textNode(String(kid));
      if (node.parentNode) node.parentNode.removeChild(node);
      node.parentNode = this;
      this.children.push(node);
    }
  }
  removeChild(child) {
    const i = this.children.indexOf(child);
    if (i >= 0) this.children.splice(i, 1);
    child.parentNode = null;
    return child;
  }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  replaceWith(node) {
    if (!this.parentNode) return;
    const i = this.parentNode.children.indexOf(this);
    this.parentNode.children[i] = node;
    node.parentNode = this.parentNode;
    this.parentNode = null;
  }

  setAttribute(k, v) {
    this.attributes[k] = String(v);
    if (k === 'id') this.id = String(v);
    // Browsers reflect these content attributes onto the property.
    if (k === 'disabled') this.disabled = true;
    if (k === 'checked') this.checked = true;
    if (k === 'value') this.value = String(v);
  }
  getAttribute(k) { return this.attributes[k]; }
  addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
  focus() { this.focused = true; document.activeElement = this; }

  dispatch(type, event) {
    for (const fn of this.listeners[type] || []) fn(Object.assign({ target: this }, event || {}));
  }

  // The names the real DOM uses, so tests read like browser code.
  dispatchEvent(event) {
    const type = (event && event.type) || '';
    this.dispatch(type, event);
    return true;
  }
  click() { this.dispatch('click', { type: 'click' }); }

  // Depth-first walk honouring a very small subset of selector syntax.
  querySelector(selector) {
    for (const part of selector.split(',').map((s) => s.trim())) {
      const found = this._find(part);
      if (found) return found;
    }
    return null;
  }
  querySelectorAll(selector) {
    const out = [];
    this._walk((n) => { if (matches(n, selector)) out.push(n); });
    return out;
  }
  _find(selector) {
    let hit = null;
    this._walk((n) => { if (!hit && matches(n, selector)) hit = n; });
    return hit;
  }
  _walk(fn) {
    for (const child of this.children) {
      if (child.nodeType !== 1) continue;
      fn(child);
      child._walk(fn);
    }
  }
}

function matches(node, selector) {
  selector = selector.trim();
  if (!selector) return false;

  // Descendant combinator: match the last part here and require an ancestor
  // chain for the rest. Without this, ".panel button" silently matches
  // nothing and a test passes by finding zero of what it was looking for,
  // which is the worst kind of green.
  if (/\s/.test(selector)) {
    const parts = selector.split(/\s+/);
    const last = parts.pop();
    if (!matches(node, last)) return false;
    let ancestor = node.parentNode;
    let remaining = parts.slice();
    while (remaining.length && ancestor) {
      if (matches(ancestor, remaining[remaining.length - 1])) remaining.pop();
      ancestor = ancestor.parentNode;
    }
    return remaining.length === 0;
  }

  // Attribute selector, optionally after a tag: input[type="checkbox"].
  const attr = selector.match(/^([A-Za-z0-9.#-]*)\[([\w-]+)(?:=["']?([^"'\]]*)["']?)?\]$/);
  if (attr) {
    const [, head, name, want] = attr;
    if (head && !matches(node, head)) return false;
    const have = node.attributes[name];
    if (have === undefined) return false;
    return want === undefined || want === '' || have === want;
  }
  // Strip the :not(...) qualifier used by the focus lookup.
  const notMatch = selector.match(/^([^:]+):not\(\[(\w+)\]\)$/);
  if (notMatch) {
    return matches(node, notMatch[1]) && node.attributes[notMatch[2]] === undefined;
  }
  if (selector.startsWith('#')) return node.id === selector.slice(1);
  if (selector.startsWith('.')) return node.classList.contains(selector.slice(1));
  const parts = selector.split('.');
  const tag = parts.shift();
  if (tag && node.tagName !== tag.toUpperCase()) return false;
  return parts.every((c) => node.classList.contains(c));
}

function textNode(value) {
  const n = new Node('#text');
  n.nodeType = 3;
  n._text = value;
  return n;
}

const document = {
  _isRoot: true,
  activeElement: null,
  listeners: {},
  body: null,
  createElement: (tag) => new Node(tag),
  createElementNS: (_ns, tag) => new Node(tag),
  createTextNode: (v) => textNode(v),
  addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); },
  dispatch(type, event) { for (const fn of document.listeners[type] || []) fn(event || {}); },
  querySelector(sel) { return document.body ? (matches(document.body, sel) ? document.body : document.body.querySelector(sel)) : null; },
  cookie: '',
};
document.addEventListener = function (type, fn) {
  (document.listeners[type] = document.listeners[type] || []).push(fn);
};

function buildPage() {
  const body = new Node('body');
  body.parentNode = document;
  const root = new Node('div');
  root.setAttribute('id', 'root');
  const toasts = new Node('div');
  toasts.setAttribute('id', 'toasts');
  const overlay = new Node('div');
  overlay.setAttribute('id', 'overlay');
  overlay.className = 'overlay hidden';
  body.append(root, toasts, overlay);
  document.body = body;
  document.activeElement = root;
  return { root, toasts, overlay };
}

module.exports = { document, Node, buildPage, matches };
