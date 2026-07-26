// Join by invite: #/join/<token>.
//
// Preview first (the endpoint always answers 200 with `valid`, so nothing leaks),
// then the signup form. This is the only route to an account.

import { h, clear } from '../dom.js';
import { t, currentLang, pickLang } from '../i18n.js';
import { api, bus } from '../api.js';
import { field, input, select, button, formError, errorMessage, colorPicker, spinner } from '../ui.js';
import { PALETTE } from '../colors.js';
import { go } from '../router.js';

const MIN_PASSWORD = 8;

export async function renderJoin({ token }) {
  const wrap = h('div', { class: 'auth' },
    h('div', { class: 'auth-brand' },
      h('h1', { class: 'auth-name' }, t('app.name')),
      h('p', { class: 'auth-tagline' }, t('app.tagline'))));
  const card = h('div', { class: 'auth-card' }, spinner());
  wrap.appendChild(card);

  let preview;
  try {
    preview = await api.invitePreview(token);
  } catch (err) {
    clear(card);
    card.appendChild(h('p', { class: 'auth-note' }, errorMessage(err)));
    return wrap;
  }

  if (!preview || !preview.valid) {
    clear(card);
    card.appendChild(h('p', { class: 'auth-note' }, t('auth.signup.invalidInvite')));
    card.appendChild(button(t('auth.login.title'), { variant: 'quiet', wide: true, onclick: () => go('/login') }));
    return wrap;
  }

  const errors = h('div', { class: 'form-errors' });
  const email = input({ type: 'email', inputmode: 'email', autocomplete: 'email', autocapitalize: 'off' });
  const password = input({ type: 'password', autocomplete: 'new-password' });
  const name = input({ autocapitalize: 'words', autocomplete: 'given-name' });
  const langSelect = select([
    { value: 'fr', label: 'Français' },
    { value: 'en', label: 'English' },
  ], { value: pickLang(currentLang()) });

  let color = PALETTE[0];
  const swatches = h('div');
  const paintSwatches = () => {
    clear(swatches);
    swatches.appendChild(colorPicker(color, (hex) => { color = hex; paintSwatches(); }));
  };
  paintSwatches();

  const submit = button(t('auth.signup.submit'), { type: 'submit', wide: true });

  const onSubmit = async (e) => {
    e.preventDefault();
    formError(errors, null);
    if (!name.value.trim() || !email.value.trim()) {
      // No catalog string for "this field is required"; the generic invalid-input
      // message is the closest existing key.
      formError(errors, t('error.invalid'));
      return;
    }
    if (password.value.length < MIN_PASSWORD) {
      formError(errors, t('auth.passwordTooShort'));
      return;
    }
    submit.disabled = true;
    try {
      await api.signup({
        invite_token: token,
        email: email.value.trim(),
        password: password.value,
        display_name: name.value.trim(),
        color,
        lang: langSelect.value,
      });
      bus.dispatchEvent(new CustomEvent('authenticated'));
    } catch (err) {
      const code = err && err.code;
      if (code === 'conflict') formError(errors, t('auth.signup.emailTaken'));
      else if (code === 'not_found' || code === 'forbidden') formError(errors, t('auth.signup.invalidInvite'));
      else formError(errors, errorMessage(err));
      submit.disabled = false;
    }
  };

  clear(card);
  card.appendChild(h('h2', { class: 'auth-title' }, t('auth.signup.title', { calendar: preview.calendar_name })));
  card.appendChild(h('form', { onsubmit: onSubmit },
    errors,
    field(t('auth.signup.displayName'), name),
    field(t('auth.email'), email),
    field(t('auth.password'), password, t('auth.passwordTooShort')),
    h('div', { class: 'field' },
      h('span', { class: 'field-label' }, t('auth.signup.color')),
      swatches),
    field(t('auth.signup.lang'), langSelect),
    submit,
    h('button', { class: 'link-btn', type: 'button', onclick: () => go('/login') }, t('auth.login.title'))));

  return wrap;
}
