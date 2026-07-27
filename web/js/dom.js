// DOM construction. Every node in this app is built here.
//
// Security contract (normative, see CONVENTIONS §6):
//   - text children are always inserted as text nodes, never parsed as HTML;
//   - attributes come from an allowlist, so no `srcdoc`/`onerror`/`formaction` smuggling;
//   - handlers are functions passed as props and attached with addEventListener —
//     the CSP ships without 'unsafe-inline', so an inline onclick= is a dead button.
// `innerHTML` appears nowhere in this codebase and must never be introduced.

const ATTRS = new Set([
  'accept', 'action', 'alt', 'autocapitalize', 'autocomplete', 'autofocus',
  'capture', 'cols', 'colspan', 'datetime', 'dir', 'download', 'enterkeyhint',
  'for', 'height', 'href', 'id', 'inputmode', 'lang', 'list', 'loading', 'max',
  'maxlength', 'min', 'minlength', 'name', 'pattern', 'placeholder', 'rel',
  'rows', 'scope', 'size', 'spellcheck', 'src', 'step', 'target', 'title',
  'type', 'value', 'width', 'wrap',
]);

// Properties (not attributes) that must be assigned to keep the DOM in sync.
const PROPS = new Set(['checked', 'disabled', 'hidden', 'multiple', 'readOnly', 'required', 'selected', 'indeterminate']);

const SAFE_SCHEMES = /^(https?:|mailto:|tel:)/i;

// safeHref rejects anything that could execute (javascript:, data:, vbscript:).
// Relative URLs, absolute paths and fragments are kept as-is.
//
// Control characters are removed first, and the *stripped* value is what gets returned.
// Both halves of that matter. The URL parser deletes ASCII tab, LF and CR from anywhere
// in a URL and strips leading C0 controls and spaces, so it decides the scheme of a
// string this function never saw: 'java<TAB>script:' matches no scheme here, falls
// through as "some kind of path", and becomes javascript: inside setAttribute. Stripping
// only for the test and then returning the original would leave that second reading
// intact — the check would be honest and the value handed on would not be.
export function safeHref(url) {
  const s = String(url == null ? '' : url).replace(/[\x00-\x20]/g, '').trim();
  if (s === '') return null;
  if (/^[a-z][a-z0-9+.-]*:/i.test(s)) return SAFE_SCHEMES.test(s) ? s : null;
  // Scheme-relative //evil.example is still http(s); everything else is a path.
  return s;
}

// Style values are simple tokens: colours, gradients, lengths. Anything with a
// url(), a semicolon or braces is dropped rather than sanitized halfway.
const STYLE_VALUE = /^[#a-zA-Z0-9 ,.%()/_-]*$/;

function setStyle(el, style) {
  for (const [k, v] of Object.entries(style)) {
    if (v == null) continue;
    const value = String(v);
    if (!STYLE_VALUE.test(value) || /url\s*\(/i.test(value)) continue;
    el.style.setProperty(k, value);
  }
}

function append(el, child) {
  if (child == null || child === false || child === true) return;
  if (Array.isArray(child)) {
    for (const c of child) append(el, c);
    return;
  }
  if (child instanceof Node) {
    el.appendChild(child);
    return;
  }
  el.appendChild(document.createTextNode(String(child)));
}

/**
 * h('div', {class: 'x', onclick: fn}, 'text', childNode) -> HTMLElement
 * Props: class, style (object), data-*, aria-*, allowlisted attributes,
 * on<event> handlers (functions), and `ref` (called with the element).
 */
export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  if (props) {
    for (const [key, value] of Object.entries(props)) {
      if (value == null || value === false) {
        if (PROPS.has(key)) el[key] = false;
        continue;
      }
      if (key === 'class') {
        el.className = Array.isArray(value) ? value.filter(Boolean).join(' ') : String(value);
      } else if (key === 'style' && typeof value === 'object') {
        setStyle(el, value);
      } else if (key === 'ref' && typeof value === 'function') {
        value(el);
      } else if (key.startsWith('on') && typeof value === 'function') {
        el.addEventListener(key.slice(2).toLowerCase(), value);
      } else if (key.startsWith('data-') || key.startsWith('aria-') || key === 'role' || key === 'tabindex') {
        el.setAttribute(key, String(value));
      } else if (key === 'href' || key === 'src' || key === 'action') {
        const safe = safeHref(value);
        if (safe !== null) el.setAttribute(key, safe);
      } else if (PROPS.has(key)) {
        el[key] = value === true ? true : value;
      } else if (ATTRS.has(key)) {
        el.setAttribute(key, String(value));
      }
      // Unknown props are dropped on purpose: the allowlist is the guardrail.
    }
  }
  append(el, children);
  return el;
}

// SVG needs createElementNS; icons are built from a fixed path table in icons.js,
// never from markup strings.
const SVG_NS = 'http://www.w3.org/2000/svg';
const SVG_ATTRS = new Set([
  'viewBox', 'd', 'fill', 'stroke', 'stroke-width', 'stroke-linecap', 'stroke-linejoin',
  'cx', 'cy', 'r', 'x', 'y', 'x1', 'y1', 'x2', 'y2', 'width', 'height', 'rx', 'ry',
  'points', 'transform', 'opacity', 'focusable', 'preserveAspectRatio',
]);

export function svg(tag, props, ...children) {
  const el = document.createElementNS(SVG_NS, tag);
  if (props) {
    for (const [key, value] of Object.entries(props)) {
      if (value == null || value === false) continue;
      if (key === 'class') el.setAttribute('class', String(value));
      else if (key.startsWith('aria-') || key === 'role') el.setAttribute(key, String(value));
      else if (key.startsWith('on') && typeof value === 'function') el.addEventListener(key.slice(2).toLowerCase(), value);
      else if (SVG_ATTRS.has(key)) el.setAttribute(key, String(value));
    }
  }
  append(el, children);
  return el;
}

export function frag(...children) {
  const f = document.createDocumentFragment();
  append(f, children);
  return f;
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
  return node;
}

export function mount(node, ...children) {
  clear(node);
  append(node, children);
  return node;
}

// on() is the same addEventListener, spelled short for imperative code paths.
export function on(node, type, fn, opts) {
  node.addEventListener(type, fn, opts);
  return node;
}
