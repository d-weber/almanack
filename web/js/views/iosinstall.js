// iOS install walkthrough.
//
// On iOS, Web Push only exists inside the home-screen app: a Safari tab can never
// receive a notification. Grandma has to be walked through Share → Add to Home
// Screen, or the reminder half of this product simply does not exist for her.
//
// Shown when the app runs in iOS Safari outside standalone mode, dismissible, and
// reachable again from Settings. The notification permission prompt is never
// raised here — only inside the installed app, on a button press.

import { h } from '../dom.js';
import { t } from '../i18n.js';
import { icon } from '../icons.js';
import { button } from '../ui.js';
import { dismissIOSInstall } from '../state.js';
import { go } from '../router.js';

export function renderIOSInstall({ dismissible = true } = {}) {
  const step = (n, text) => h('li', { class: 'install-step' },
    h('span', { class: 'install-num' }, String(n)),
    h('span', null, text));

  return h('div', { class: 'install' },
    h('div', { class: 'install-card' },
      h('div', { class: 'install-icon' }, icon('share')),
      h('h1', { class: 'install-title' }, t('ios.install.title')),
      h('p', { class: 'install-why' }, t('ios.install.why')),
      h('ol', { class: 'install-steps' },
        step(1, t('ios.install.step1')),
        step(2, t('ios.install.step2')),
        step(3, t('ios.install.step3'))),
      dismissible
        ? button(t('ios.install.later'), {
          variant: 'quiet',
          wide: true,
          onclick: () => { dismissIOSInstall(); go('/'); },
        })
        : null));
}
