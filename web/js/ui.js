// Shared widgets: form rows, sheets, dialogs, toasts, avatars, colour pickers.
// Everything here is built with h(), so every label passed in is inserted as text.

import { h, frag, clear } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { errorKey } from './api.js';
import { normalizeHex, readableOn, initialsOf, PALETTE } from './colors.js';
import { avatarURL } from './state.js';

// ---------------------------------------------------------------------------
// Layout
// ---------------------------------------------------------------------------

/** Full-screen sub-page with a back arrow and optional trailing actions. */
export function screen({ title, onBack, actions = [], scroll = true }, ...children) {
  return h('div', { class: 'screen' },
    h('header', { class: 'screen-bar' },
      onBack
        ? h('button', { class: 'icon-btn', type: 'button', onclick: onBack, 'aria-label': t('action.back') }, icon('chevronLeft'))
        : h('span', { class: 'icon-btn-spacer' }),
      h('h1', { class: 'screen-title' }, title),
      h('div', { class: 'screen-actions' }, ...actions)),
    h('div', { class: scroll ? 'screen-body scroll' : 'screen-body' }, ...children));
}

export function section(title, ...children) {
  return h('section', { class: 'card' },
    title ? h('h2', { class: 'card-title' }, title) : null,
    ...children);
}

export function listRow({ onclick, href, leading, title, subtitle, trailing, danger }) {
  const inner = [
    leading ? h('span', { class: 'row-leading' }, leading) : null,
    h('span', { class: 'row-main' },
      h('span', { class: 'row-title' }, title),
      subtitle ? h('span', { class: 'row-sub' }, subtitle) : null),
    trailing ? h('span', { class: 'row-trailing' }, trailing) : null,
  ];
  if (href) return h('a', { class: ['row', danger ? 'danger' : ''].join(' ').trim(), href }, ...inner);
  if (onclick) return h('button', { class: ['row', danger ? 'danger' : ''].join(' ').trim(), type: 'button', onclick }, ...inner);
  return h('div', { class: ['row', danger ? 'danger' : ''].join(' ').trim() }, ...inner);
}

export function emptyState(text) {
  return h('p', { class: 'empty' }, text);
}

export function spinner() {
  return h('div', { class: 'spinner', role: 'status', 'aria-label': t('action.loading') },
    h('span', { class: 'spinner-dot' }), h('span', { class: 'spinner-dot' }), h('span', { class: 'spinner-dot' }));
}

export function banner(text, { kind = 'info', actionLabel, onAction } = {}) {
  return h('div', { class: `banner banner-${kind}`, role: kind === 'warn' ? 'alert' : 'status' },
    icon(kind === 'warn' ? 'warning' : 'bell', { class: 'banner-icon' }),
    h('span', { class: 'banner-text' }, text),
    actionLabel && onAction
      ? h('button', { class: 'btn btn-quiet btn-small', type: 'button', onclick: onAction }, actionLabel)
      : null);
}

// ---------------------------------------------------------------------------
// Form controls
// ---------------------------------------------------------------------------

let uid = 0;
function nextId(prefix) {
  uid += 1;
  return `${prefix}-${uid}`;
}

export function field(labelText, control, hint) {
  const id = control.id || nextId('f');
  control.id = id;
  return h('div', { class: 'field' },
    h('label', { class: 'field-label', for: id }, labelText),
    control,
    hint ? h('p', { class: 'field-hint' }, hint) : null);
}

export function input(props = {}) {
  return h('input', { class: 'input', type: 'text', ...props });
}

export function textarea(props = {}) {
  return h('textarea', { class: 'input textarea', rows: 4, ...props });
}

export function select(options, { value, onchange, ...rest } = {}) {
  const el = h('select', { class: 'input select', onchange, ...rest },
    ...options.map((o) => h('option', { value: String(o.value), selected: String(o.value) === String(value) }, o.label)));
  el.value = String(value);
  return el;
}

export function button(label, { onclick, variant = 'primary', type = 'button', iconName, disabled, wide } = {}) {
  return h('button', {
    class: ['btn', `btn-${variant}`, wide ? 'btn-wide' : ''].filter(Boolean).join(' '),
    type, onclick, disabled: Boolean(disabled),
  }, iconName ? icon(iconName) : null, h('span', null, label));
}

export function iconButton(name, { onclick, label, variant = 'quiet', pressed } = {}) {
  const props = {
    class: `icon-btn icon-btn-${variant}`,
    type: 'button',
    onclick,
    'aria-label': label,
    title: label,
  };
  if (pressed !== undefined) props['aria-pressed'] = String(Boolean(pressed));
  return h('button', props, icon(name));
}

export function toggleRow(labelText, checked, onchange, hint) {
  const box = h('input', { type: 'checkbox', class: 'switch-input', checked: Boolean(checked), onchange: (e) => onchange(e.target.checked) });
  const id = nextId('t');
  box.id = id;
  return h('div', { class: 'toggle-row' },
    h('div', { class: 'toggle-text' },
      h('label', { class: 'toggle-label', for: id }, labelText),
      hint ? h('p', { class: 'field-hint' }, hint) : null),
    h('label', { class: 'switch', for: id }, box, h('span', { class: 'switch-track' }, h('span', { class: 'switch-thumb' }))));
}

export function segmented(options, value, onpick) {
  return h('div', { class: 'segmented', role: 'tablist' },
    ...options.map((o) => h('button', {
      class: ['segment', String(o.value) === String(value) ? 'is-active' : ''].filter(Boolean).join(' '),
      type: 'button',
      role: 'tab',
      'aria-selected': String(String(o.value) === String(value)),
      onclick: () => onpick(o.value),
    }, o.label)));
}

/** Colour swatch grid used for member, calendar and label colours. */
export function colorPicker(value, onpick, palette = PALETTE) {
  const current = normalizeHex(value);
  const grid = h('div', { class: 'swatches', role: 'radiogroup' });
  const paint = () => {
    clear(grid);
    for (const c of palette) {
      const hex = normalizeHex(c);
      const active = hex === current;
      grid.appendChild(h('button', {
        class: ['swatch', active ? 'is-active' : ''].filter(Boolean).join(' '),
        type: 'button',
        role: 'radio',
        'aria-checked': String(active),
        'aria-label': hex,
        style: { '--c': hex, '--c-on': readableOn(hex) },
        onclick: () => onpick(hex),
      }, active ? icon('check', { class: 'swatch-check' }) : null));
    }
  };
  paint();
  return grid;
}

// ---------------------------------------------------------------------------
// People
// ---------------------------------------------------------------------------

/** Avatar bubble: photo when the member has one, initials on their colour otherwise. */
export function avatar(member, size = 'md') {
  const color = normalizeHex(member && member.color);
  const el = h('span', {
    class: `avatar avatar-${size}`,
    style: { '--c': color, '--c-on': readableOn(color) },
    title: member ? member.display_name : '',
  }, h('span', { class: 'avatar-initials' }, initialsOf(member && member.display_name)));
  if (member && member.has_avatar) {
    const img = h('img', {
      class: 'avatar-img',
      src: avatarURL(member.user_id != null ? member.user_id : member.id),
      alt: '',
      loading: 'lazy',
      onerror: () => img.remove(),
    });
    el.appendChild(img);
  }
  return el;
}

export function avatarStack(members, size = 'sm') {
  return h('span', { class: 'avatar-stack' }, ...members.slice(0, 5).map((m) => avatar(m, size)));
}

/** Multi-select of calendar members, used by the editor and search filters. */
export function memberSelector(members, selectedIds, onchange) {
  const selected = new Set(selectedIds.map(Number));
  const wrap = h('div', { class: 'member-select' });
  const paint = () => {
    clear(wrap);
    for (const m of members) {
      const on = selected.has(m.user_id);
      wrap.appendChild(h('button', {
        class: ['member-pill', on ? 'is-active' : ''].filter(Boolean).join(' '),
        type: 'button',
        'aria-pressed': String(on),
        style: { '--c': normalizeHex(m.color), '--c-on': readableOn(normalizeHex(m.color)) },
        onclick: () => {
          if (selected.has(m.user_id)) selected.delete(m.user_id);
          else selected.add(m.user_id);
          paint();
          onchange(Array.from(selected));
        },
      }, avatar(m, 'sm'), h('span', null, m.display_name)));
    }
  };
  paint();
  return wrap;
}

// ---------------------------------------------------------------------------
// Overlays
// ---------------------------------------------------------------------------

// Every overlay currently open, in the order they were opened. Overlays deliberately
// stack — a confirmation over a sheet is a real thing this app does, and the Tab
// handling below reasons about the stack — but they belong to the screen that opened
// them, and a navigation has to take them down. Without that, moving between two day
// routes left two day sheets on top of each other: closing the front one revealed a
// sheet for the day you had just left.
const openOverlays = new Set();

/** Close every open overlay. Called when a navigation replaces the screen beneath them. */
export function closeOverlays() {
  // A copy, because closing removes from the set as it goes.
  for (const close of [...openOverlays]) close();
}

// The width at which this app is a desktop: the sidebar exists, the event panel opens
// beside the calendar, and the top bar carries controls a phone puts elsewhere. One
// definition, imported by everything that has to agree with it — it is already stated a
// second time in the stylesheet, and a third would be one too many.
export const DESKTOP = window.matchMedia('(min-width: 900px)');

function modalRoot() {
  let root = document.getElementById('modal-root');
  if (!root) {
    root = h('div', { id: 'modal-root' });
    document.body.appendChild(root);
  }
  return root;
}

// Everything inside a panel that the keyboard can reach. Deliberately a superset of
// what the app builds today, because openOverlay is handed arbitrary content.
const FOCUSABLE = [
  'a[href]', 'area[href]', 'button', 'input', 'select', 'textarea',
  '[tabindex]', '[contenteditable="true"]',
].join(', ');

/**
 * The panel's focusable elements, in tab order.
 *
 * Asked again on every Tab rather than once when the overlay opens: panels repaint
 * themselves — the colour grid redraws its swatches on every pick, the member pills on
 * every toggle — so a list captured at open time would send the keyboard to buttons
 * that no longer exist. A hidden or disabled control is not reachable and must not be
 * counted, or Tab appears to do nothing at the ends of the cycle.
 */
function focusableIn(panel) {
  return Array.from(panel.querySelectorAll(FOCUSABLE)).filter((el) => (
    !el.disabled
    && el.getAttribute('tabindex') !== '-1'
    && el.getAttribute('aria-hidden') !== 'true'
    && (el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0)
  ));
}

/**
 * Bottom sheet / centred dialog. Returns a close() function.
 * Escape and a backdrop tap both dismiss unless `dismissible` is false.
 *
 * Modal here means modal to the keyboard as well as to the eye: Tab cycles inside the
 * panel instead of walking into the page behind it, the control that opened the overlay
 * gets the keyboard back whichever way the overlay is closed, and `role="dialog"` is
 * given the name it is meaningless without.
 */
export function openOverlay(content, { onClose, dismissible = true, variant = 'sheet' } = {}) {
  const root = modalRoot();
  // Whatever asked the question — a Delete button, a day cell, a row. Read before the
  // panel exists, because opening one moves the focus into it.
  const opener = document.activeElement;
  let closed = false;

  const close = () => {
    if (closed) return;
    closed = true;
    openOverlays.delete(close);
    document.removeEventListener('keydown', onKey, true);
    overlay.remove();
    if (!root.firstChild) document.body.classList.remove('has-overlay');
    // Every way out lands here — Escape, the backdrop, and the panel's own buttons all
    // call close() — so the keyboard is handed back once, in one place. `isConnected`
    // is the guard for the case where answering rebuilt the screen underneath: the
    // opener is gone, and there is nothing to give it back to.
    if (opener && opener.isConnected && typeof opener.focus === 'function') opener.focus();
    if (onClose) onClose();
  };

  const onKey = (e) => {
    if (e.key === 'Escape' && dismissible) {
      e.stopPropagation();
      close();
      return;
    }
    if (e.key !== 'Tab') return;
    // Only the overlay on top traps. Every open overlay has a listener on the document,
    // and one underneath must not drag the keyboard out of the panel in front of it.
    const stack = root.querySelectorAll('.overlay');
    if (stack.length && stack[stack.length - 1] !== overlay) return;

    const items = focusableIn(panel);
    if (items.length === 0) {
      // A panel with nothing to focus still holds the keyboard, on itself: letting Tab
      // through would leave the reader in a page they cannot see past the backdrop.
      e.preventDefault();
      panel.focus();
      return;
    }
    const first = items[0];
    const last = items[items.length - 1];
    const active = document.activeElement;
    if (!panel.contains(active)) {
      // The focused element was repainted away, or a backdrop click cleared it.
      e.preventDefault();
      (e.shiftKey ? last : first).focus();
    } else if (e.shiftKey && active === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && active === last) {
      e.preventDefault();
      first.focus();
    }
    // With a single focusable element first and last are the same one, so both of those
    // branches refocus it: Tab stays put rather than escaping.
  };

  const panel = h('div', {
    class: `overlay-panel overlay-${variant}`,
    role: 'dialog',
    'aria-modal': 'true',
    // So the panel can hold the keyboard itself when it has no focusable content.
    tabindex: '-1',
  }, typeof content === 'function' ? content(close) : content);

  // A dialog with no accessible name is announced as "dialog" and nothing else. Every
  // overlay in this app draws its question as a heading, so the name is that heading
  // rather than a second copy of it that could drift from what is on screen. An overlay
  // built without one — none today, but this takes arbitrary content — falls back to a
  // generic name from the catalogue, which is still an answer to "what is this?".
  const heading = panel.querySelector('h1, h2, h3, h4, h5, h6');
  if (heading) {
    if (!heading.id) heading.id = nextId('overlay-title');
    panel.setAttribute('aria-labelledby', heading.id);
  } else {
    panel.setAttribute('aria-label', t('a11y.dialog'));
  }

  const overlay = h('div', {
    class: 'overlay',
    onclick: (e) => { if (dismissible && e.target === overlay) close(); },
  }, panel);

  root.appendChild(overlay);
  openOverlays.add(close);
  document.body.classList.add('has-overlay');
  document.addEventListener('keydown', onKey, true);
  // Focus once the panel is in the document, so that a control hidden by CSS is not
  // mistaken for a reachable one.
  (focusableIn(panel)[0] || panel).focus();
  return close;
}

/** Yes/no confirmation. Resolves true when confirmed. */
export function confirmDialog({ title, message, confirmLabel, danger = false }) {
  return new Promise((resolve) => {
    let done = false;
    const finish = (value) => { if (!done) { done = true; resolve(value); } };
    const close = openOverlay((dismiss) => h('div', { class: 'dialog' },
      h('h2', { class: 'dialog-title' }, title),
      message ? h('p', { class: 'dialog-message' }, message) : null,
      h('div', { class: 'dialog-actions' },
        button(t('action.cancel'), { variant: 'quiet', onclick: () => { finish(false); dismiss(); } }),
        button(confirmLabel || t('action.confirm'), {
          variant: danger ? 'danger' : 'primary',
          onclick: () => { finish(true); dismiss(); },
        }))), { variant: 'dialog', onClose: () => finish(false) });
    void close;
  });
}

/** Single choice from a list — the recurrence scope question uses this. */
export function chooseDialog(title, options) {
  return new Promise((resolve) => {
    let done = false;
    const finish = (value) => { if (!done) { done = true; resolve(value); } };
    openOverlay((dismiss) => h('div', { class: 'dialog' },
      h('h2', { class: 'dialog-title' }, title),
      h('div', { class: 'choice-list' },
        ...options.map((o) => h('button', {
          class: 'choice',
          type: 'button',
          onclick: () => { finish(o.value); dismiss(); },
        }, o.label))),
      h('div', { class: 'dialog-actions' },
        button(t('action.cancel'), { variant: 'quiet', onclick: () => { finish(null); dismiss(); } }))),
    { variant: 'dialog', onClose: () => finish(null) });
  });
}

let toastTimer = null;

export function toast(text) {
  const root = modalRoot();
  const existing = root.querySelector('.toast');
  if (existing) existing.remove();
  const node = h('div', { class: 'toast', role: 'status' }, text);
  root.appendChild(node);
  if (toastTimer) clearTimeout(toastTimer);
  toastTimer = setTimeout(() => node.remove(), 3200);
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

export function errorMessage(err) {
  return t(errorKey(err));
}

export function errorBox(err, onRetry) {
  return h('div', { class: 'error-box', role: 'alert' },
    h('p', null, errorMessage(err)),
    onRetry ? button(t('action.retry'), { variant: 'quiet', onclick: onRetry }) : null);
}

/** Inline form error line; pass null to clear. */
export function formError(node, text) {
  clear(node);
  if (text) node.appendChild(h('p', { class: 'form-error', role: 'alert' }, text));
  return node;
}

export { frag };
