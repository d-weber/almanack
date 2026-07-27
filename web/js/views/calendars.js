// Calendar management: create, rename, recolour, labels, members, invite links,
// leave/delete. Permissions are flat by design — only the creator may remove
// someone, and only a sole member may delete a calendar.

import { h, clear } from '../dom.js';
import { t } from '../i18n.js';
import { icon } from '../icons.js';
import { api } from '../api.js';
import {
  state, calendarById, labelsOf, membersOf, refreshMe, invalidateRange, calendarImageURL,
} from '../state.js';
import { formatDateShort } from '../dates.js';
import { normalizeHex, readableOn, PALETTE } from '../colors.js';
import {
  screen, section, field, input, button, toggleRow, colorPicker, avatar, listRow,
  confirmDialog, openOverlay, toast, errorMessage, emptyState,
} from '../ui.js';
import { go, back, reload } from '../router.js';
import {
  resizeImage, MAX_UPLOAD_BYTES, MAX_SOURCE_BYTES,
} from '../images.js';

function colorButton(value, onpick) {
  const hex = normalizeHex(value);
  return h('button', {
    class: 'color-button',
    type: 'button',
    'aria-label': t('calendar.color'),
    style: { '--c': hex, '--c-on': readableOn(hex) },
    onclick: () => {
      const close = openOverlay((dismiss) => h('div', { class: 'dialog' },
        h('h2', { class: 'dialog-title' }, t('calendar.color')),
        colorPicker(hex, (picked) => { dismiss(); onpick(picked); }, PALETTE),
        h('div', { class: 'dialog-actions' },
          button(t('action.cancel'), { variant: 'quiet', onclick: () => dismiss() }))), { variant: 'dialog' });
      void close;
    },
  });
}

async function guard(fn) {
  try {
    await fn();
  } catch (err) {
    toast(errorMessage(err));
    return false;
  }
  return true;
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

export function renderCalendarList() {
  const body = h('div');

  const paint = () => {
    clear(body);
    body.appendChild(section(null, ...state.calendars.map((cal) => listRow({
      leading: h('span', { class: 'dot', style: { '--c': normalizeHex(cal.color) } }),
      title: cal.name,
      subtitle: (cal.members || []).map((m) => m.display_name).join(', '),
      trailing: icon('chevronRight'),
      onclick: () => go(`/calendars/${cal.id}`),
    })), state.calendars.length ? null : emptyState(t('calendar.new'))));

    body.appendChild(button(t('calendar.new'), { iconName: 'plus', wide: true, onclick: openCreate }));
  };

  const openCreate = () => {
    let color = PALETTE[6];
    const name = input({ autocapitalize: 'sentences' });
    const swatchHolder = h('div');
    const paintSwatch = () => {
      clear(swatchHolder);
      swatchHolder.appendChild(colorPicker(color, (hex) => { color = hex; paintSwatch(); }));
    };
    paintSwatch();

    openOverlay((dismiss) => h('div', { class: 'dialog' },
      h('h2', { class: 'dialog-title' }, t('calendar.new')),
      field(t('calendar.name'), name),
      h('div', { class: 'field' }, h('span', { class: 'field-label' }, t('calendar.color')), swatchHolder),
      h('div', { class: 'dialog-actions' },
        button(t('action.cancel'), { variant: 'quiet', onclick: () => dismiss() }),
        button(t('action.save'), {
          onclick: async () => {
            if (!name.value.trim()) return;
            const ok = await guard(async () => {
              await api.createCalendar({ name: name.value.trim(), color });
              await refreshMe();
              invalidateRange();
            });
            if (ok) { dismiss(); paint(); }
          },
        }))), { variant: 'sheet' });
  };

  paint();
  return screen({ title: t('nav.calendar'), onBack: () => back('/settings') }, body);
}

// ---------------------------------------------------------------------------
// One calendar
// ---------------------------------------------------------------------------

/**
 * The calendar's picture: what identifies it in the sidebar. Optional — without one
 * the calendar shows a tinted calendar glyph, which is why there is no empty state
 * to design here, only a swap.
 */
function imageField(cal) {
  const preview = h('div', { class: 'cal-image-preview', style: { '--c': normalizeHex(cal.color) } },
    cal.has_image
      ? h('img', { class: 'cal-image-img', src: `${calendarImageURL(cal.id)}?v=${Date.now()}`, alt: '' })
      : icon('calendar'));

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
        if (blob.size > MAX_UPLOAD_BYTES) throw Object.assign(new Error('too large'), { code: 'invalid' });
        await api.putCalendarImage(cal.id, blob);
        await refreshMe();
        reload();
      });
    },
  });

  const actions = h('div', { class: 'cal-image-actions' },
    button(t(cal.has_image ? 'calendar.image.change' : 'calendar.image'), {
      variant: 'quiet', iconName: 'camera', onclick: () => fileInput.click(),
    }),
    cal.has_image
      ? button(t('calendar.image.remove'), {
        variant: 'quiet',
        onclick: async () => {
          await guard(async () => {
            await api.deleteCalendarImage(cal.id);
            await refreshMe();
            reload();
          });
        },
      })
      : null,
    fileInput);

  return h('div', { class: 'field' },
    h('span', { class: 'field-label' }, t('calendar.image')),
    h('div', { class: 'cal-image-row' }, preview, actions),
    h('p', { class: 'field-hint' }, t('calendar.image.hint')));
}

export function renderCalendarDetail({ id }) {
  const cal = calendarById(id);
  if (!cal) {
    return screen({ title: t('error.not_found'), onBack: () => back('/calendars') },
      emptyState(t('error.not_found')));
  }

  const me = state.user;
  const isCreator = me && cal.creator_id === me.id;
  const members = membersOf(cal.id);
  const alone = members.length <= 1;
  const body = h('div');

  // -- identity
  const nameInput = input({
    value: cal.name,
    autocapitalize: 'sentences',
    onchange: async (e) => {
      const value = e.target.value.trim();
      if (!value || value === cal.name) return;
      await guard(async () => {
        await api.patchCalendar(cal.id, { name: value });
        await refreshMe();
        reload();
      });
    },
  });

  body.appendChild(section(null,
    imageField(cal),
    field(t('calendar.name'), nameInput),
    h('div', { class: 'field' },
      h('span', { class: 'field-label' }, t('calendar.color')),
      colorButton(cal.color, async (hex) => {
        await guard(async () => {
          await api.patchCalendar(cal.id, { color: hex });
          await refreshMe();
          invalidateRange();
          reload();
        });
      }))));

  // -- notifications for this calendar
  const membership = cal.membership || { muted: false, participating_only: false };
  body.appendChild(section(t('prefs.notifications'),
    toggleRow(t('calendar.mute'), membership.muted, async (checked) => {
      await guard(async () => {
        await api.patchMembership(cal.id, { muted: checked });
        await refreshMe();
      });
    }),
    toggleRow(t('calendar.participatingOnly'), membership.participating_only, async (checked) => {
      await guard(async () => {
        await api.patchMembership(cal.id, { participating_only: checked });
        await refreshMe();
      });
    })));

  // -- labels
  const labelRows = labelsOf(cal.id).map((label) => h('div', { class: 'label-row' },
    colorButton(label.color, async (hex) => {
      await guard(async () => {
        await api.patchLabel(cal.id, label.id, { color: hex });
        await refreshMe();
        invalidateRange();
        reload();
      });
    }),
    input({
      value: label.name,
      onchange: async (e) => {
        const value = e.target.value.trim();
        if (!value || value === label.name) return;
        await guard(async () => {
          await api.patchLabel(cal.id, label.id, { name: value });
          await refreshMe();
        });
      },
    })));
  body.appendChild(section(t('calendar.labels'), ...labelRows));

  // -- members
  body.appendChild(section(t('calendar.members'), ...members.map((m) => listRow({
    leading: avatar(m, 'md'),
    title: m.display_name,
    subtitle: m.user_id === cal.creator_id ? t('calendar.creator') : '',
    trailing: isCreator && me && m.user_id !== me.id
      ? h('button', {
        class: 'icon-btn icon-btn-quiet',
        type: 'button',
        'aria-label': t('calendar.removeMember', { name: m.display_name }),
        onclick: async () => {
          const ok = await confirmDialog({
            title: t('calendar.removeMember.confirm', { name: m.display_name }),
            confirmLabel: t('action.confirm'),
            danger: true,
          });
          if (!ok) return;
          await guard(async () => {
            await api.removeMember(cal.id, m.user_id);
            await refreshMe();
            reload();
          });
        },
      }, icon('close'))
      : null,
  }))));

  // -- invites
  const inviteBox = h('div', { class: 'invite-box' });
  const loadInvites = async () => {
    clear(inviteBox);
    try {
      const data = await api.listInvites(cal.id);
      const invites = Array.isArray(data) ? data : (data && data.invites) || [];
      if (!invites.length) {
        inviteBox.appendChild(h('p', { class: 'field-hint' }, t('calendar.invite.hint')));
        return;
      }
      for (const inv of invites) {
        inviteBox.appendChild(listRow({
          title: t('calendar.invite.expires', { date: formatDateShort(String(inv.expires_at).slice(0, 10)) }),
          trailing: h('button', {
            class: 'btn btn-quiet btn-small',
            type: 'button',
            onclick: async () => {
              await guard(async () => {
                await api.revokeInvite(inv.id);
                await loadInvites();
              });
            },
          }, t('calendar.invite.revoke')),
        }));
      }
    } catch (err) {
      inviteBox.appendChild(h('p', { class: 'field-hint' }, errorMessage(err)));
    }
  };
  loadInvites();

  const showInvite = (invite) => {
    // Built here rather than taken from the server: this is a hash-routed app, and a
  // path-only invite link lands the invitee on the login screen with no way to sign
  // up — which, signup being invite-only, is the end of the road for them.
  const url = `${location.origin}/#/join/${invite.token}`;
    const urlField = input({ value: url, readOnly: true });
    openOverlay((dismiss) => h('div', { class: 'dialog' },
      h('h2', { class: 'dialog-title' }, t('calendar.invite.link')),
      h('p', { class: 'field-hint' }, t('calendar.invite.hint')),
      urlField,
      invite.expires_at
        ? h('p', { class: 'field-hint' }, t('calendar.invite.expires', { date: formatDateShort(String(invite.expires_at).slice(0, 10)) }))
        : null,
      h('div', { class: 'dialog-actions' },
        button(t('action.close'), { variant: 'quiet', onclick: () => dismiss() }),
        button(t('action.copy'), {
          iconName: 'copy',
          onclick: async () => {
            try {
              if (navigator.clipboard && navigator.clipboard.writeText) await navigator.clipboard.writeText(url);
              else { urlField.select(); document.execCommand('copy'); }
              toast(t('calendar.invite.copied'));
            } catch (_) {
              urlField.select();
            }
          },
        }))), { variant: 'sheet' });
  };

  body.appendChild(section(t('calendar.invite'),
    button(t('calendar.invite'), {
      iconName: 'share',
      wide: true,
      onclick: async () => {
        await guard(async () => {
          const invite = await api.createInvite(cal.id);
          showInvite(invite);
          await loadInvites();
        });
      },
    }),
    inviteBox));

  // -- leaving / deleting
  body.appendChild(section(null,
    button(t('calendar.leave'), {
      variant: 'quiet',
      wide: true,
      onclick: async () => {
        const ok = await confirmDialog({
          title: t('calendar.leave.confirm', { name: cal.name }),
          confirmLabel: t('action.confirm'),
          danger: true,
        });
        if (!ok) return;
        await guard(async () => {
          await api.leaveCalendar(cal.id);
          await refreshMe();
          invalidateRange();
          go('/calendars');
        });
      },
    }),
    button(t('calendar.delete'), {
      variant: 'danger',
      wide: true,
      disabled: !alone,
      onclick: async () => {
        const ok = await confirmDialog({
          title: t('calendar.delete.confirm', { name: cal.name }),
          confirmLabel: t('action.delete'),
          danger: true,
        });
        if (!ok) return;
        await guard(async () => {
          await api.deleteCalendar(cal.id);
          await refreshMe();
          invalidateRange();
          go('/calendars');
        });
      },
    }),
    alone ? null : h('p', { class: 'field-hint' }, t('calendar.delete.notAlone'))));

  return screen({ title: cal.name, onBack: () => back('/calendars') }, body);
}
