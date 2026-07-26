// Settings: profile, colour mode, notification preferences, push health,
// calendars and the About block.

import { h, clear } from '../dom.js';
import { t, loadLang } from '../i18n.js';
import { icon } from '../icons.js';
import { api } from '../api.js';
import {
  state, refreshMe, setColorBy, rememberLang, clearSession, invalidateRange, avatarURL,
} from '../state.js';
import { setTimeFormat, formatDateShort, instantDate } from '../dates.js';
import { normalizeHex, PALETTE } from '../colors.js';
import {
  section, field, input, select, button, toggleRow, segmented, colorPicker, avatar,
  listRow, banner, toast, confirmDialog, errorMessage, emptyState,
} from '../ui.js';
import {
  pushSupported, notificationPermission, currentSubscription, enablePush, disablePush,
  needsIOSInstall, uaLabel,
} from '../push.js';
import { go, reload } from '../router.js';
import {
  resizeImage, MAX_UPLOAD_BYTES as MAX_AVATAR_BYTES, MAX_SOURCE_BYTES,
} from '../images.js';


async function guard(fn) {
  try {
    await fn();
    return true;
  } catch (err) {
    toast(errorMessage(err));
    return false;
  }
}

async function patchPrefs(partial) {
  const updated = await api.patchPrefs(partial);
  state.prefs = updated && typeof updated === 'object' ? updated : Object.assign({}, state.prefs, partial);
}

export function renderSettings() {
  const user = state.user;
  const prefs = state.prefs || {};
  const body = h('div');

  if (!user) return h('div', { class: 'screen' }, emptyState(t('error.unauthorized')));

  // -- profile --------------------------------------------------------------
  const avatarHolder = h('div', { class: 'avatar-edit' }, avatar({
    user_id: user.id, display_name: user.display_name, color: user.color, has_avatar: user.has_avatar,
  }, 'xl'));

  const fileInput = h('input', {
    type: 'file',
    accept: 'image/jpeg,image/png',
    class: 'visually-hidden',
    onchange: async (e) => {
      const file = e.target.files && e.target.files[0];
      e.target.value = '';
      if (!file) return;
      if (file.size > MAX_SOURCE_BYTES) { toast(t('prefs.avatar.tooLarge')); return; }
      await guard(async () => {
        const blob = await resizeImage(file);
        if (blob.size > MAX_AVATAR_BYTES) throw Object.assign(new Error('too large'), { code: 'invalid' });
        await api.putAvatar(blob);
        await refreshMe();
        // Bust the avatar cache for this session.
        const img = avatarHolder.querySelector('.avatar-img');
        if (img) img.src = `${avatarURL(user.id)}?v=${Date.now()}`;
        reload();
      });
    },
  });

  const nameInput = input({
    value: user.display_name,
    autocapitalize: 'words',
    onchange: async (e) => {
      const value = e.target.value.trim();
      if (!value || value === user.display_name) return;
      await guard(async () => { await api.patchMe({ display_name: value }); await refreshMe(); });
    },
  });

  const colorHolder = h('div');
  const paintColor = () => {
    clear(colorHolder);
    colorHolder.appendChild(colorPicker(user.color, async (hex) => {
      await guard(async () => {
        await api.patchMe({ color: hex });
        await refreshMe();
        invalidateRange();
        reload();
      });
    }, PALETTE));
  };
  paintColor();

  body.appendChild(section(t('prefs.profile'),
    h('div', { class: 'profile-head' },
      avatarHolder,
      h('div', { class: 'profile-actions' },
        button(t('prefs.avatar.change'), { variant: 'quiet', iconName: 'camera', onclick: () => fileInput.click() }),
        user.has_avatar
          ? button(t('prefs.avatar.remove'), {
            variant: 'quiet',
            onclick: async () => {
              await guard(async () => { await api.deleteAvatar(); await refreshMe(); reload(); });
            },
          })
          : null),
      fileInput),
    field(t('prefs.displayName'), nameInput),
    h('div', { class: 'field' }, h('span', { class: 'field-label' }, t('prefs.color')), colorHolder),
    field(t('prefs.language'), select([
      { value: 'fr', label: 'Français' },
      { value: 'en', label: 'English' },
    ], {
      value: user.lang,
      onchange: async (e) => {
        const lang = e.target.value;
        await guard(async () => {
          await api.patchMe({ lang });
          rememberLang(lang);
          await loadLang(lang);
          await refreshMe();
          reload();
        });
      },
    })),
    field(t('prefs.weekStart'), select([
      { value: 1, label: t('prefs.weekStart.monday') },
      { value: 0, label: t('prefs.weekStart.sunday') },
    ], {
      value: Number(user.week_start),
      onchange: async (e) => {
        await guard(async () => { await api.patchMe({ week_start: Number(e.target.value) }); await refreshMe(); reload(); });
      },
    })),
    field(t('prefs.timeFormat'), select([
      { value: '24h', label: '24 h' },
      { value: '12h', label: 'AM / PM' },
    ], {
      value: user.time_format,
      onchange: async (e) => {
        const fmt = e.target.value;
        await guard(async () => {
          await api.patchMe({ time_format: fmt });
          setTimeFormat(fmt);
          await refreshMe();
          reload();
        });
      },
    }))));

  // -- colour mode ----------------------------------------------------------
  // Light/dark follows the OS (prefers-color-scheme). A manual override exists in
  // the stylesheet via [data-theme] but has no catalog strings to label it, so it
  // is deliberately not exposed here.
  body.appendChild(section(t('prefs.colorBy'),
    segmented([
      { value: 'label', label: t('prefs.colorBy.label') },
      { value: 'person', label: t('prefs.colorBy.person') },
    ], state.colorBy, (mode) => { setColorBy(mode); reload(); })));

  // -- password -------------------------------------------------------------
  const currentPw = input({ type: 'password', autocomplete: 'current-password' });
  const newPw = input({ type: 'password', autocomplete: 'new-password' });
  body.appendChild(section(t('prefs.password'),
    field(t('prefs.password.current'), currentPw),
    field(t('prefs.password.new'), newPw),
    button(t('action.save'), {
      variant: 'quiet',
      onclick: async () => {
        if (newPw.value.length < 8) { toast(t('auth.passwordTooShort')); return; }
        try {
          await api.patchMe({ current_password: currentPw.value, new_password: newPw.value });
          currentPw.value = '';
          newPw.value = '';
          toast(t('prefs.password.changed'));
        } catch (err) {
          const code = err && err.code;
          toast(code === 'unauthorized' || code === 'forbidden' ? t('prefs.password.wrong') : errorMessage(err));
        }
      },
    })));

  // -- notifications --------------------------------------------------------
  const health = prefs.push_health || {};
  const notif = section(t('prefs.notifications'));

  if (health.stale) {
    notif.appendChild(banner(t('prefs.push.repair'), { kind: 'warn' }));
  }

  notif.appendChild(toggleRow(t('prefs.digest'), prefs.digest_enabled, async (checked) => {
    await guard(() => patchPrefs({ digest_enabled: checked }));
    reload();
  }));
  if (prefs.digest_enabled) {
    notif.appendChild(field(t('prefs.digestTime'), input({
      type: 'time',
      value: prefs.digest_time || '07:30',
      onchange: async (e) => { await guard(() => patchPrefs({ digest_time: e.target.value })); },
    })));
    notif.appendChild(toggleRow(t('prefs.digestOnEmpty'), prefs.digest_on_empty, async (checked) => {
      await guard(() => patchPrefs({ digest_on_empty: checked }));
    }));
  }

  notif.appendChild(toggleRow(t('prefs.activityPush'), prefs.activity_push, async (checked) => {
    await guard(() => patchPrefs({ activity_push: checked }));
    reload();
  }));
  notif.appendChild(toggleRow(t('prefs.summaryMode'), prefs.daily_summary_mode, async (checked) => {
    await guard(() => patchPrefs({ daily_summary_mode: checked }));
    reload();
  }));
  if (prefs.daily_summary_mode) {
    notif.appendChild(field(t('prefs.summaryTime'), input({
      type: 'time',
      value: prefs.summary_time || '20:00',
      onchange: async (e) => { await guard(() => patchPrefs({ summary_time: e.target.value })); },
    })));
  }

  notif.appendChild(toggleRow(t('prefs.emailReminders'), prefs.email_reminders, async (checked) => {
    await guard(() => patchPrefs({ email_reminders: checked }));
  }, t('prefs.emailReminders.hint')));
  notif.appendChild(toggleRow(t('prefs.emailDigest'), prefs.email_digest, async (checked) => {
    await guard(() => patchPrefs({ email_digest: checked }));
  }));
  body.appendChild(notif);

  // -- push on this device --------------------------------------------------
  body.appendChild(pushSection(health));

  // -- calendars ------------------------------------------------------------
  body.appendChild(section(t('nav.calendar'),
    ...state.calendars.map((cal) => listRow({
      leading: h('span', { class: 'dot', style: { '--c': normalizeHex(cal.color) } }),
      title: cal.name,
      trailing: icon('chevronRight'),
      onclick: () => go(`/calendars/${cal.id}`),
    })),
    button(t('calendar.new'), { variant: 'quiet', iconName: 'plus', wide: true, onclick: () => go('/calendars') })));

  // -- about ----------------------------------------------------------------
  body.appendChild(section(t('prefs.about'),
    h('p', { class: 'field-hint' }, t('prefs.version', { version: state.appVersion || '—' })),
    h('p', { class: 'field-hint' }, `${t('app.name')} — ${t('app.tagline')}`),
    // AGPL-3.0 section 13: people using this over a network are entitled to its
    // source. A link here is the simplest way to offer it.
    h('p', { class: 'field-hint' },
      t('about.licence'),
      state.config && state.config.source_url
        ? h('span', null, ' ', h('a', { href: state.config.source_url, rel: 'noopener' }, t('about.source')))
        : null),
    button(t('auth.logout'), {
      variant: 'danger',
      iconName: 'logout',
      wide: true,
      onclick: async () => {
        const ok = await confirmDialog({ title: t('auth.logout'), confirmLabel: t('action.confirm') });
        if (!ok) return;
        // Unsubscribe first: a logged-out shared device must stop receiving the
        // family's events.
        await disablePush();
        try { await api.logout(); } catch (_) { /* leaving anyway */ }
        clearSession();
        go('/login');
      },
    })));

  return h('div', { class: 'screen' },
    h('header', { class: 'screen-bar' }, h('h1', { class: 'screen-title' }, t('prefs.title'))),
    h('div', { class: 'screen-body scroll' }, body));
}

function pushSection(health) {
  const box = section(t('prefs.push.enable'));
  const status = h('div', { class: 'push-status' });
  box.appendChild(status);

  const devices = [];
  if (health && typeof health.devices === 'number') {
    devices.push(listRow({
      leading: icon('bell'),
      title: t('prefs.devices'),
      subtitle: health.last_confirmed_at
        ? formatDateShort(instantDate(health.last_confirmed_at))
        : '',
      trailing: h('span', { class: 'count' }, String(health.devices)),
    }));
  }
  devices.push(listRow({ leading: icon('bell'), title: uaLabel() }));

  const paint = async () => {
    clear(status);
    if (!pushSupported()) {
      status.appendChild(h('p', { class: 'field-hint' }, t('prefs.push.unsupported')));
      return;
    }
    if (needsIOSInstall()) {
      status.appendChild(h('p', { class: 'field-hint' }, t('ios.install.why')));
      status.appendChild(button(t('ios.install.title'), { variant: 'quiet', wide: true, onclick: () => go('/ios-install') }));
      return;
    }
    const permission = notificationPermission();
    if (permission === 'denied') {
      status.appendChild(h('p', { class: 'field-hint' }, t('prefs.push.blocked')));
      return;
    }
    const sub = await currentSubscription();
    if (permission === 'granted' && sub) {
      status.appendChild(h('p', { class: 'field-hint' }, t('prefs.push.enabled')));
      status.appendChild(h('div', { class: 'button-row' },
        button(t('prefs.push.test'), {
          variant: 'quiet',
          onclick: async () => {
            try {
              const res = await api.pushTest();
              toast(t('prefs.push.testSent', { n: res && res.sent != null ? res.sent : 0 }));
            } catch (err) {
              toast(errorMessage(err));
            }
          },
        }),
        button(t('action.delete'), {
          variant: 'quiet',
          onclick: async () => { await disablePush(); paint(); },
        })));
      status.appendChild(h('div', { class: 'device-list' }, ...devices));
      return;
    }
    // Permission is requested here and nowhere else: a user gesture, inside the
    // installed app.
    status.appendChild(button(t('prefs.push.enable'), {
      iconName: 'bell',
      wide: true,
      onclick: async () => {
        const key = state.config && state.config.vapid_public_key;
        if (!key) { toast(t('prefs.push.unsupported')); return; }
        try {
          const result = await enablePush(key);
          if (result === 'blocked') toast(t('prefs.push.blocked'));
          else if (result === 'unsupported') toast(t('prefs.push.unsupported'));
          else toast(t('prefs.push.enabled'));
        } catch (err) {
          toast(errorMessage(err));
        }
        paint();
      },
    }));
  };

  paint();
  return box;
}
