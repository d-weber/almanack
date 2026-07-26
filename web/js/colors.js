// Colour handling: the feature the family will judge this app on.
//
// Two reading modes (a per-device setting, prefs.colorBy):
//   - by label   — the chip body takes the label colour, and a strip on its edge
//                  still shows who the event concerns;
//   - by person  — the chip body takes the participants' colours, and the label
//                  survives as a small dot.
// Either way "whose event is this" is answered by the strip, without reading text.
//
// Chips never paint a solid colour behind body text. They expose `--c` (the full
// colour, used for the strip and borders) and `--c-rgb` (the triplet), and the
// stylesheet mixes the background alpha per theme. That keeps contrast honest in
// both light and dark without per-chip luminance maths.

const HEX = /^#[0-9a-fA-F]{6}$/;
const SHORT_HEX = /^#[0-9a-fA-F]{3}$/;

export const NEUTRAL = '#7a8899';

/** Any server-provided colour is validated before it reaches CSS. */
export function normalizeHex(value, fallback = NEUTRAL) {
  const s = String(value == null ? '' : value).trim();
  if (HEX.test(s)) return s.toLowerCase();
  if (SHORT_HEX.test(s)) return `#${s[1]}${s[1]}${s[2]}${s[2]}${s[3]}${s[3]}`.toLowerCase();
  return fallback;
}

export function rgbOf(hex) {
  const c = normalizeHex(hex);
  return [
    parseInt(c.slice(1, 3), 16),
    parseInt(c.slice(3, 5), 16),
    parseInt(c.slice(5, 7), 16),
  ];
}

/** '59, 125, 221' — for rgba(var(--c-rgb), .18) in the stylesheet. */
export function rgbTriplet(hex) {
  return rgbOf(hex).join(', ');
}

function srgbToLinear(v) {
  const x = v / 255;
  return x <= 0.03928 ? x / 12.92 : Math.pow((x + 0.055) / 1.055, 2.4);
}

export function luminance(hex) {
  const [r, g, b] = rgbOf(hex).map(srgbToLinear);
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

/** Text colour to put on a fully saturated swatch. */
export function readableOn(hex) {
  return luminance(hex) > 0.42 ? '#16181d' : '#ffffff';
}

/**
 * Multi-participant strip: hard-stopped bands, one per person, top to bottom.
 * Values are normalized hex, so the generated gradient is a safe style value.
 */
export function stripBackground(colors) {
  const list = colors.map((c) => normalizeHex(c));
  if (list.length === 0) return normalizeHex(NEUTRAL);
  if (list.length === 1) return list[0];
  const capped = list.slice(0, 4);
  const step = 100 / capped.length;
  const stops = capped.map((c, i) => `${c} ${(i * step).toFixed(2)}% ${((i + 1) * step).toFixed(2)}%`);
  return `linear-gradient(to bottom, ${stops.join(', ')})`;
}

/**
 * Colour decision for one occurrence.
 * `mode` is 'label' or 'person'; `people` are the participants' member records.
 * Falls back to the label colour when a person-coloured event has no participants,
 * and to the calendar colour when the label is missing.
 */
export function occurrenceColors(occ, mode, people) {
  const labelColor = normalizeHex(occ.label_color || occ.calendar_color, NEUTRAL);
  const personColors = people.map((p) => normalizeHex(p.color, NEUTRAL));
  const main = mode === 'person' && personColors.length ? personColors[0] : labelColor;
  return {
    main,
    label: labelColor,
    strip: stripBackground(personColors.length ? personColors : [main]),
    people: personColors,
  };
}

/** Style object for a chip, bar or list row. */
export function chipStyle(colors) {
  return {
    '--c': colors.main,
    '--c-rgb': rgbTriplet(colors.main),
    '--c-strip': colors.strip,
    '--c-label': colors.label,
    '--c-on': readableOn(colors.main),
  };
}

/** Two-letter initials for an avatar bubble. */
export function initialsOf(name) {
  const parts = String(name || '').trim().split(/\s+/).filter(Boolean);
  if (parts.length === 0) return '?';
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}

/**
 * Picker palette. 24 colours, evenly spread around the wheel and kept away from
 * both extremes of lightness so they stay legible on white and on near-black.
 */
export const PALETTE = [
  '#c0392b', '#e8743b', '#e0a800', '#8bbf3d', '#3aa757', '#2fa8a0',
  '#3b7ddd', '#3f51b5', '#7952b3', '#b34b8c', '#d6558a', '#8d6e63',
  '#e05252', '#f08a4b', '#f2c14e', '#a4c85a', '#57b878', '#4cc3bd',
  '#5b9bea', '#6673cc', '#9370c9', '#c86fa8', '#e37ba3', '#a1887f',
];
