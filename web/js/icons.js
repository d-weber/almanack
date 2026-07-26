// Icon set, drawn as SVG paths built through the h()/svg() helpers — no markup
// strings, no icon font, no sprite sheet to fetch. 24×24 grid, 1.7px stroke.

import { svg } from './dom.js';

const PATHS = {
  chevronLeft: ['M15 5l-7 7 7 7'],
  chevronRight: ['M9 5l7 7-7 7'],
  chevronDown: ['M5 9l7 7 7-7'],
  plus: ['M12 5v14', 'M5 12h14'],
  check: ['M4 12.5l5 5 11-11'],
  close: ['M6 6l12 12', 'M18 6L6 18'],
  calendar: ['M4 7a2 2 0 012-2h12a2 2 0 012 2v12a2 2 0 01-2 2H6a2 2 0 01-2-2z', 'M4 10h16', 'M8 3v4', 'M16 3v4'],
  search: ['M11 4a7 7 0 100 14 7 7 0 000-14z', 'M16.5 16.5L21 21'],
  activity: ['M12 4a6 6 0 016 6v4l1.5 3h-15L6 14v-4a6 6 0 016-6z', 'M10 20a2 2 0 004 0'],
  settings: ['M12 9a3 3 0 100 6 3 3 0 000-6z', 'M4 12a8 8 0 01.2-1.7l-1.6-1.5 2-3.4 2 .8A8 8 0 019.4 5l.4-2.1h4.4l.4 2.1a8 8 0 012.8 1.2l2-.8 2 3.4-1.6 1.5a8 8 0 010 3.4l1.6 1.5-2 3.4-2-.8a8 8 0 01-2.8 1.2l-.4 2.1H9.8l-.4-2.1a8 8 0 01-2.8-1.2l-2 .8-2-3.4 1.6-1.5A8 8 0 014 12z'],
  pencil: ['M4 20h4l10-10a2.8 2.8 0 10-4-4L4 16z', 'M13.5 6.5l4 4'],
  trash: ['M5 7h14', 'M10 11v6', 'M14 11v6', 'M6 7l1 12a2 2 0 002 2h6a2 2 0 002-2l1-12', 'M9 7V5a2 2 0 012-2h2a2 2 0 012 2v2'],
  copy: ['M9 9h9a2 2 0 012 2v9a2 2 0 01-2 2H9a2 2 0 01-2-2v-9a2 2 0 012-2z', 'M15 5H6a2 2 0 00-2 2v9'],
  clock: ['M12 4a8 8 0 100 16 8 8 0 000-16z', 'M12 8v4.5l3 2'],
  pin: ['M12 21s7-6.2 7-11a7 7 0 10-14 0c0 4.8 7 11 7 11z', 'M12 8a2.5 2.5 0 100 5 2.5 2.5 0 000-5z'],
  link: ['M10 13a4 4 0 006 .5l2-2a4 4 0 10-5.7-5.7L11 7', 'M14 11a4 4 0 00-6-.5l-2 2a4 4 0 105.7 5.7L13 17'],
  notes: ['M6 4h12v16H6z', 'M9 9h6', 'M9 13h6', 'M9 17h3'],
  repeat: ['M4 10a6 6 0 016-6h7', 'M14 1l3 3-3 3', 'M20 14a6 6 0 01-6 6H7', 'M10 23l-3-3 3-3'],
  people: ['M9 11a3.5 3.5 0 100-7 3.5 3.5 0 000 7z', 'M2.5 20a6.5 6.5 0 0113 0', 'M16 5.2a3.5 3.5 0 010 6.6', 'M17.5 14.2A6.5 6.5 0 0121.5 20'],
  bell: ['M12 4a6 6 0 016 6v4l1.5 3h-15L6 14v-4a6 6 0 016-6z'],
  tag: ['M4 12V5a1 1 0 011-1h7l8 8-8 8z', 'M8.5 8.5h.01'],
  logout: ['M15 5V4a2 2 0 00-2-2H6a2 2 0 00-2 2v16a2 2 0 002 2h7a2 2 0 002-2v-1', 'M11 12h10', 'M18 9l3 3-3 3'],
  share: ['M12 15V4', 'M8.5 7.5L12 4l3.5 3.5', 'M5 13v6a2 2 0 002 2h10a2 2 0 002-2v-6'],
  warning: ['M12 4l9 16H3z', 'M12 10v4', 'M12 17h.01'],
  camera: ['M4 8h3l1.5-2h7L17 8h3v12H4z', 'M12 11a3.5 3.5 0 100 7 3.5 3.5 0 000-7z'],
  sun: ['M12 8a4 4 0 100 8 4 4 0 000-8z', 'M12 2v2', 'M12 20v2', 'M2 12h2', 'M20 12h2',
    'M4.9 4.9l1.4 1.4', 'M17.7 17.7l1.4 1.4', 'M19.1 4.9l-1.4 1.4', 'M6.3 17.7l-1.4 1.4'],
  moon: ['M20 14.5A8.5 8.5 0 019.5 4a8.5 8.5 0 1010.5 10.5z'],
  // "Auto" is the sun and moon sharing one disc: the theme follows the system.
  themeAuto: ['M12 4a8 8 0 100 16 8 8 0 000-16z', 'M12 4v16a8 8 0 000-16z'],
  sidebarCollapse: ['M4 5h16v14H4z', 'M10 5v14', 'M16.5 10.5L14 12l2.5 1.5'],
  sidebarExpand: ['M4 5h16v14H4z', 'M10 5v14', 'M13.5 10.5L16 12l-2.5 1.5'],
};

/** icon('plus', {class: 'x'}) -> <svg> */
export function icon(name, props = {}) {
  const paths = PATHS[name] || PATHS.calendar;
  return svg('svg', {
    class: ['icon', props.class].filter(Boolean).join(' '),
    viewBox: '0 0 24 24',
    fill: 'none',
    stroke: 'currentColor',
    'stroke-width': '1.7',
    'stroke-linecap': 'round',
    'stroke-linejoin': 'round',
    'aria-hidden': 'true',
    focusable: 'false',
  }, ...paths.map((d) => svg('path', { d })));
}

export function hasIcon(name) {
  return Object.prototype.hasOwnProperty.call(PATHS, name);
}
