// Colour handling: the feature the family will judge this app on.
//
// Two reading modes (a per-device setting, prefs.colorBy) decide where an event's
// colour comes from:
//   - by label   — the label's colour, with the participants' colours available to
//                  whatever wants them;
//   - by person  — the participants' colours, and the label survives as a dot.
//
// An event is drawn in that colour rather than in a wash of it, so `--c-on` carries
// the text colour to use over it: readableOn() measures the WCAG contrast of black
// and white against the actual colour and returns whichever wins. `--c-rgb` is the
// same colour as a triplet, for the tint the phone paints behind a timed event.

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
/**
 * Ink for text drawn on a coloured background.
 *
 * Picks whichever of the two actually contrasts better, rather than choosing by a
 * luminance threshold. A fixed cutoff reads as reasonable and is not: it preferred
 * white on mid-tone colours where black is the far better choice, so a mid orange
 * carried white text at 2.4:1 — well under the 4.5:1 that makes small text legible —
 * while the same rule gave black on lime at 11.7:1.
 */
export function readableOn(hex) {
  // True black rather than the interface's near-black: on a saturated fill the
  // difference is imperceptible, but it is worth about 0.7 of a contrast ratio, which
  // is exactly what carries mid-tone reds and blues over the 4.5:1 line.
  const dark = '#000000';
  const light = '#ffffff';
  return contrastRatio(hex, dark) >= contrastRatio(hex, light) ? dark : light;
}

/** WCAG relative-luminance contrast ratio between two colours, 1:1 to 21:1. */
export function contrastRatio(a, b) {
  const la = luminance(a);
  const lb = luminance(b);
  const hi = Math.max(la, lb);
  const lo = Math.min(la, lb);
  return (hi + 0.05) / (lo + 0.05);
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
    people: personColors,
  };
}

/** Style object for a chip, bar or list row. */
export function chipStyle(colors) {
  return {
    '--c': colors.main,
    '--c-rgb': rgbTriplet(colors.main),
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
