// Login, password-reset request, password-reset confirmation.
// These screens render without the app shell — there is no session yet.

import { h, clear } from '../dom.js';
import { t } from '../i18n.js';
import { api, bus } from '../api.js';
import { field, input, button, formError, errorMessage } from '../ui.js';
import { go } from '../router.js';

const MIN_PASSWORD = 8;

function authShell(title, ...children) {
  return h('div', { class: 'auth' },
    h('div', { class: 'auth-brand' },
      h('h1', { class: 'auth-name' }, t('app.name')),
      h('p', { class: 'auth-tagline' }, t('app.tagline'))),
    h('div', { class: 'auth-card' },
      h('h2', { class: 'auth-title' }, title),
      ...children));
}

function authenticated() {
  bus.dispatchEvent(new CustomEvent('authenticated'));
}

export function renderLogin() {
  const errors = h('div', { class: 'form-errors' });
  const email = input({ type: 'email', inputmode: 'email', autocomplete: 'username', autocapitalize: 'off', enterkeyhint: 'next' });
  const password = input({ type: 'password', autocomplete: 'current-password', enterkeyhint: 'go' });
  const submit = button(t('auth.login.submit'), { type: 'submit', wide: true });

  const onSubmit = async (e) => {
    e.preventDefault();
    formError(errors, null);
    submit.disabled = true;
    try {
      await api.login(email.value.trim(), password.value);
      authenticated();
    } catch (err) {
      const code = err && err.code;
      if (code === 'rate_limited') formError(errors, t('auth.login.throttled'));
      else if (code === 'unauthorized' || code === 'invalid') formError(errors, t('auth.login.failed'));
      else formError(errors, errorMessage(err));
      submit.disabled = false;
    }
  };

  const form = h('form', { onsubmit: onSubmit },
    errors,
    field(t('auth.email'), email),
    field(t('auth.password'), password),
    submit,
    h('button', { class: 'link-btn', type: 'button', onclick: () => go('/forgot') }, t('auth.forgot')));

  return authShell(t('auth.login.title'), form);
}

export function renderForgot() {
  const errors = h('div', { class: 'form-errors' });
  const email = input({ type: 'email', inputmode: 'email', autocomplete: 'username', autocapitalize: 'off' });
  const submit = button(t('auth.reset.request'), { type: 'submit', wide: true });
  const body = h('div');

  const onSubmit = async (e) => {
    e.preventDefault();
    formError(errors, null);
    submit.disabled = true;
    try {
      await api.resetRequest(email.value.trim());
    } catch (err) {
      // The endpoint never reveals whether the address exists; a transport error
      // is the only thing worth reporting.
      if (err && err.code === 'network') {
        formError(errors, errorMessage(err));
        submit.disabled = false;
        return;
      }
    }
    clear(body);
    body.appendChild(h('p', { class: 'auth-note' }, t('auth.reset.sent')));
    body.appendChild(button(t('auth.login.title'), { variant: 'quiet', wide: true, onclick: () => go('/login') }));
  };

  body.appendChild(h('form', { onsubmit: onSubmit },
    errors,
    field(t('auth.email'), email),
    submit,
    h('button', { class: 'link-btn', type: 'button', onclick: () => go('/login') }, t('action.back'))));

  return authShell(t('auth.reset.title'), body);
}

export function renderReset({ token }) {
  const errors = h('div', { class: 'form-errors' });
  const password = input({ type: 'password', autocomplete: 'new-password' });
  const submit = button(t('auth.reset.submit'), { type: 'submit', wide: true });
  const body = h('div');

  const onSubmit = async (e) => {
    e.preventDefault();
    formError(errors, null);
    if (password.value.length < MIN_PASSWORD) {
      formError(errors, t('auth.passwordTooShort'));
      return;
    }
    submit.disabled = true;
    try {
      await api.resetConfirm(token, password.value);
      clear(body);
      body.appendChild(h('p', { class: 'auth-note' }, t('auth.reset.done')));
      body.appendChild(button(t('auth.login.submit'), { wide: true, onclick: () => go('/login') }));
    } catch (err) {
      const code = err && err.code;
      if (code === 'not_found' || code === 'invalid' || code === 'unauthorized') formError(errors, t('auth.reset.invalid'));
      else formError(errors, errorMessage(err));
      submit.disabled = false;
    }
  };

  body.appendChild(h('form', { onsubmit: onSubmit },
    errors,
    field(t('auth.reset.newPassword'), password),
    submit));

  return authShell(t('auth.reset.title'), body);
}
