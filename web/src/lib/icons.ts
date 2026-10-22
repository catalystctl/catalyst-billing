// Icons.
//
// A closed union of names, each a constant path string. That is why `icon()` may set
// innerHTML: no caller can pass user data through it. Anything that needs an arbitrary
// string (an invoice description, a provider error) renders as text instead.

import { raw } from "./dom";

const PATHS = {
  home: '<path d="M4 10.5 12 4l8 6.5V20a1 1 0 0 1-1 1h-4v-6H9v6H5a1 1 0 0 1-1-1z"/>',
  users:
    '<path d="M16 20v-1.5a3.5 3.5 0 0 0-3.5-3.5h-5A3.5 3.5 0 0 0 4 18.5V20"/><circle cx="10" cy="8" r="3.2"/><path d="M20 20v-1.4a3.4 3.4 0 0 0-2.6-3.3M15.4 5.2a3.2 3.2 0 0 1 0 6"/>',
  invoice:
    '<path d="M6 3h8l4 4v14H6z"/><path d="M14 3v4h4"/><path d="M9 12h6M9 16h4"/>',
  server:
    '<rect x="3" y="4" width="18" height="7" rx="1.6"/><rect x="3" y="13" width="18" height="7" rx="1.6"/><path d="M7 7.5h.01M7 16.5h.01"/>',
  bag: '<path d="M5 8h14l-1 12H6z"/><path d="M9 8V6.5a3 3 0 0 1 6 0V8"/>',
  card: '<rect x="2.5" y="5" width="19" height="14" rx="2.2"/><path d="M2.5 10h19"/><path d="M6 15h4"/>',
  activity: '<path d="M3 12h4l2.5-6 4 12 2.5-6h5"/>',
  radio: '<circle cx="12" cy="12" r="2.2"/><path d="M7.8 7.8a6 6 0 0 0 0 8.4M16.2 7.8a6 6 0 0 1 0 8.4M4.9 4.9a10 10 0 0 0 0 14.2M19.1 4.9a10 10 0 0 1 0 14.2"/>',
  box: '<path d="M12 3 3.5 7.5v9L12 21l8.5-4.5v-9z"/><path d="M3.5 7.5 12 12l8.5-4.5M12 12v9"/>',
  checkCircle: '<circle cx="12" cy="12" r="9"/><path d="m8.5 12.2 2.4 2.4 4.6-5"/>',
  shield: '<path d="M12 3 5 6v6c0 4 3 7.2 7 9 4-1.8 7-5 7-9V6z"/>',
  shieldCheck: '<path d="M12 3 5 6v6c0 4 3 7.2 7 9 4-1.8 7-5 7-9V6z"/><path d="m9 12 2 2 4-4"/>',
  key: '<circle cx="8" cy="8" r="4"/><path d="m11 11 9 9M17 17l2-2M14 14l2-2"/>',
  sliders: '<path d="M4 7h10M18 7h2M4 17h4M12 17h8"/><circle cx="16" cy="7" r="2"/><circle cx="10" cy="17" r="2"/>',
  search: '<circle cx="11" cy="11" r="6.5"/><path d="m16 16 4 4"/>',
  chevronRight: '<path d="m9 5 7 7-7 7"/>',
  chevronLeft: '<path d="m15 5-7 7 7 7"/>',
  chevronDown: '<path d="m5 9 7 7 7-7"/>',
  chevronUp: '<path d="m5 15 7-7 7 7"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  minus: '<path d="M5 12h14"/>',
  x: '<path d="m6 6 12 12M18 6 6 18"/>',
  check: '<path d="m5 12.5 4.5 4.5L19 7"/>',
  alertTriangle: '<path d="M12 4 2.8 20h18.4z"/><path d="M12 9.5v4.5M12 17.2v.2"/>',
  alertCircle: '<circle cx="12" cy="12" r="9"/><path d="M12 7.5v5M12 16.2v.2"/>',
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 7.8v.2"/>',
  external: '<path d="M14 4h6v6M20 4l-9 9"/><path d="M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/>',
  copy: '<rect x="9" y="9" width="11" height="11" rx="1.8"/><path d="M15 5.5A1.5 1.5 0 0 0 13.5 4H5.5A1.5 1.5 0 0 0 4 5.5v8A1.5 1.5 0 0 0 5.5 15"/>',
  download: '<path d="M12 3v12M7.5 10.5 12 15l4.5-4.5M4 20h16"/>',
  refresh: '<path d="M20 11a8 8 0 1 0-.6 4"/><path d="M20 4v7h-7"/>',
  more: '<circle cx="6" cy="12" r="1.3"/><circle cx="12" cy="12" r="1.3"/><circle cx="18" cy="12" r="1.3"/>',
  menu: '<path d="M4 7h16M4 12h16M4 17h16"/>',
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.2 5.2l1.4 1.4M17.4 17.4l1.4 1.4M18.8 5.2l-1.4 1.4M6.6 17.4l-1.4 1.4"/>',
  moon: '<path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5z"/>',
  logout: '<path d="M9 20H5a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1h4"/><path d="M16 8l4 4-4 4M20 12H9"/>',
  mail: '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="m3.5 6.5 8.5 6 8.5-6"/>',
  lock: '<rect x="4.5" y="10" width="15" height="10" rx="2"/><path d="M8 10V7.5a4 4 0 0 1 8 0V10"/>',
  arrowRight: '<path d="M4 12h15M13 6l6 6-6 6"/>',
  arrowLeft: '<path d="M20 12H5M11 6l-6 6 6 6"/>',
  arrowUpRight: '<path d="M7 17 17 7M9 7h8v8"/>',
  filter: '<path d="M3 5h18l-7 8v6l-4 2v-8z"/>',
  calendar: '<rect x="3.5" y="5" width="17" height="15" rx="2"/><path d="M3.5 10h17M8 3.5v3M16 3.5v3"/>',
  dollar: '<path d="M12 3v18"/><path d="M16 7.5c0-1.7-1.8-3-4-3s-4 1.3-4 3 1.8 2.7 4 3.2 4 1.5 4 3.3-1.8 3-4 3-4-1.3-4-3"/>',
  trendUp: '<path d="M3 17l6-6 4 4 7-8"/><path d="M15 7h5v5"/>',
  trendDown: '<path d="M3 7l6 6 4-4 7 8"/><path d="M15 17h5v-5"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5.2l3.2 2"/>',
  trash: '<path d="M4 7h16M9 7V4.8h6V7M6.5 7l1 13h9l1-13"/><path d="M10 11v5M14 11v5"/>',
  edit: '<path d="M4 20h4L20 8l-4-4L4 16z"/><path d="m14.5 5.5 4 4"/>',
  eye: '<path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12z"/><circle cx="12" cy="12" r="3"/>',
  zap: '<path d="M13 2 4 14h6l-1 8 9-12h-6z"/>',
  wifi: '<path d="M2.5 9a14 14 0 0 1 19 0M5.5 12.5a9.5 9.5 0 0 1 13 0M8.5 16a5 5 0 0 1 7 0"/><path d="M12 19.5v.2"/>',
  wifiOff: '<path d="M2 2l20 20"/><path d="M2.5 9a14 14 0 0 1 5-3M12 6c3 0 5.6 1 7.5 3M5.5 12.5a9.5 9.5 0 0 1 4-2.2M14.5 10.4a9.5 9.5 0 0 1 4 2.1M8.5 16a5 5 0 0 1 4.5-1.3"/>',
  terminal: '<rect x="3" y="4.5" width="18" height="15" rx="2"/><path d="m7 10 3 2.5-3 2.5M13 15h4"/>',
  book: '<path d="M4 5.5A1.5 1.5 0 0 1 5.5 4H19v16H5.5A1.5 1.5 0 0 1 4 18.5z"/><path d="M4 17h15"/>',
  pauseCircle: '<circle cx="12" cy="12" r="9"/><path d="M10 9v6M14 9v6"/>',
  playCircle: '<circle cx="12" cy="12" r="9"/><path d="M10 8.5 16 12l-6 3.5z"/>',
  send: '<path d="M21 3 10.5 13.5M21 3l-7 18-3.5-7.5L3 10z"/>',
  link: '<path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 1 0-5.7-5.7L11.5 6.8"/><path d="M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 1 0 5.7 5.7l1.5-1.5"/>',
  user: '<circle cx="12" cy="8" r="3.6"/><path d="M4.5 20a7.5 7.5 0 0 1 15 0"/>',
  building: '<rect x="4" y="3" width="10" height="18" rx="1.4"/><path d="M14 9h5.5v12H14"/><path d="M7 7h1M10 7h1M7 11h1M10 11h1M7 15h1M10 15h1"/>',
  layers: '<path d="m12 3 9 5-9 5-9-5z"/><path d="m3 13 9 5 9-5"/>',
  database: '<ellipse cx="12" cy="6" rx="8" ry="3"/><path d="M4 6v6c0 1.7 3.6 3 8 3s8-1.3 8-3V6M4 12v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6"/>',
  flag: '<path d="M5 21V4M5 4h11l-1.5 4L16 12H5"/>',
  printer: '<path d="M7 8V3.5h10V8"/><rect x="3.5" y="8" width="17" height="8" rx="2"/><path d="M7 14h10v6.5H7z"/>',
  bell: '<path d="M6 9a6 6 0 0 1 12 0c0 5 1.5 6 1.5 6h-15S6 14 6 9"/><path d="M10 19a2 2 0 0 0 4 0"/>',
  sparkles: '<path d="m12 3 1.8 4.7L18.5 9.5l-4.7 1.8L12 16l-1.8-4.7L5.5 9.5l4.7-1.8z"/><path d="M18.5 15.5l.8 2 2 .8-2 .8-.8 2-.8-2-2-.8 2-.8z"/>',
  inbox: '<path d="M3.5 13.5h4l1.5 3h6l1.5-3h4"/><path d="M5.5 4.5h13l2 9v6a1 1 0 0 1-1 1H4.5a1 1 0 0 1-1-1v-6z"/>',
  circle: '<circle cx="12" cy="12" r="7"/>',
  dot: '<circle cx="12" cy="12" r="3" fill="currentColor" stroke="none"/>',
  undo: '<path d="M4 10h10a5 5 0 0 1 0 10h-3"/><path d="m8 6-4 4 4 4"/>',
  rotate: '<path d="M20 8v5h-5"/><path d="M19.4 13a8 8 0 1 1-1.9-8.4L20 7"/>',
  globe: '<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.5 2.6 3.8 5.7 3.8 9S14.5 18.4 12 21c-2.5-2.6-3.8-5.7-3.8-9S9.5 5.6 12 3z"/>',
  logoutBox: '<path d="M14 4h4a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1h-4"/><path d="M10 8 6 12l4 4M6 12h9"/>',
  // Commerce and support icons. They exist so a navigation entry never renders a blank
  // glyph: `icon()` looks its path up in a closed union, and a missing name is a type error
  // rather than a silently empty square.
  tag: '<path d="M11.5 3.5H20a.5.5 0 0 1 .5.5v8.5L11 22 2 13z"/><circle cx="16.5" cy="7.5" r="1.4"/>',
  wallet: '<rect x="3" y="6" width="18" height="13" rx="2.2"/><path d="M3 10h18M16.5 14.5h2"/>',
  message: '<path d="M4 5.5h16v10H8.5L4 19z"/>',
  percent: '<path d="M6 18 18 6"/><circle cx="7.5" cy="7.5" r="2"/><circle cx="16.5" cy="16.5" r="2"/>',
  settings: '<circle cx="12" cy="12" r="3"/><path d="M12 3.5v2M12 18.5v2M3.5 12h2M18.5 12h2M6 6l1.4 1.4M16.6 16.6 18 18M18 6l-1.4 1.4M7.4 16.6 6 18"/>',
} as const;

export type IconName = keyof typeof PATHS;

const SVG_NS = "http://www.w3.org/2000/svg";

/** Builds an icon element. `size` is in pixels; the stroke scales with the viewBox. */
export function icon(name: IconName, size = 16, className?: string): SVGElement {
  const el = document.createElementNS(SVG_NS, "svg");
  el.setAttribute("viewBox", "0 0 24 24");
  el.setAttribute("width", String(size));
  el.setAttribute("height", String(size));
  el.setAttribute("fill", "none");
  el.setAttribute("stroke", "currentColor");
  el.setAttribute("stroke-width", "1.7");
  el.setAttribute("stroke-linecap", "round");
  el.setAttribute("stroke-linejoin", "round");
  el.setAttribute("aria-hidden", "true");
  if (className) el.setAttribute("class", className);
  el.innerHTML = raw(PATHS[name]).html;
  return el;
}

/**
 * The wordmark's glyph: the Catalyst mark, traced from the parent logo
 * (catalystctl.com/logo-512.png) and kept in scripts/brand/mark.path.
 *
 * It is the parent's artwork in the parent's orange, unscaled and unredrawn: a sub-brand
 * that restyled its parent's logo would be claiming to be a different company. Only the
 * composition changes — this product puts "Billing" where the panel puts "Hosting".
 */
export const BRAND_MARK_PATH =
  "M240.5 469.1C236.1 466.5 216.3 455.0 196.5 443.5C176.7 432.1 143.6 412.8 123.0 400.7C102.4 388.5 81.0 376.0 75.6 372.9C70.1 369.7 65.3 366.7 64.8 366.2C63.9 365.3 63.6 292.1 64.5 291.2C64.7 290.9 65.5 291.2 66.2 291.7C66.9 292.3 87.5 304.2 112.0 318.2C136.5 332.3 175.2 354.5 198.0 367.7C220.8 380.8 242.0 393.0 245.1 394.8L250.6 397.9L264.1 390.4C271.5 386.2 289.5 376.0 304.2 367.8L331.0 352.8L348.7 360.6C391.7 379.3 402.6 384.2 403.3 384.9C404.4 385.9 403.6 386.4 344.5 420.3C261.3 468.0 250.6 474.0 249.5 473.9C249.0 473.9 244.9 471.7 240.5 469.1ZM415.5 367.0C408.9 363.9 387.3 353.5 367.6 344.1L331.7 326.9L327.6 328.9C322.9 331.1 277.3 353.3 263.0 360.4C257.8 362.9 252.9 365.0 252.0 365.0C250.0 364.9 217.4 342.3 203.7 331.4C191.3 321.6 184.4 313.7 179.4 303.9C175.9 296.9 172.4 285.3 173.4 284.3C173.7 284.0 188.0 292.2 205.2 302.5C222.4 312.9 236.9 321.3 237.5 321.3C238.1 321.3 254.7 312.7 274.5 302.3C330.5 272.8 344.4 265.5 345.5 265.2C346.1 265.1 355.8 270.0 367.0 276.3C378.3 282.5 398.9 293.9 412.8 301.6L438.0 315.5L437.8 341.3L437.5 367.1L432.5 369.9L427.5 372.8L415.5 367.0ZM109.0 289.4C90.6 279.3 72.9 269.5 69.7 267.7L63.8 264.5L63.9 209.0L63.9 153.5L102.2 131.1C137.8 110.3 151.2 102.5 223.4 60.6C238.2 52.0 250.7 45.0 251.2 45.0C251.7 45.0 270.1 55.6 292.3 68.6C314.4 81.7 345.6 99.9 361.5 109.3C377.4 118.7 397.0 130.1 404.8 134.8C412.7 139.4 418.8 143.6 418.3 144.0C417.9 144.5 401.2 152.2 381.3 161.3L345.0 177.7L335.3 172.1C329.9 168.9 308.7 156.8 288.2 145.1L251.0 123.7L230.2 135.7C218.8 142.2 194.8 156.1 176.8 166.6L144.0 185.5L144.0 211.8L144.0 238.2L148.2 235.8C150.6 234.4 158.0 230.3 164.7 226.6L176.9 219.8L177.2 213.7L177.5 207.6L213.5 187.3C233.3 176.2 250.1 167.0 250.9 167.0C251.6 167.0 263.1 173.3 276.4 180.9C289.6 188.6 301.7 195.4 303.2 196.1C304.8 196.8 306.0 197.6 306.0 198.0C306.0 198.3 304.0 200.6 301.5 203.0C299.0 205.4 297.0 207.7 297.0 208.0C297.0 208.8 307.7 214.0 309.2 214.0C310.4 214.0 354.3 193.5 411.4 166.3C425.7 159.5 437.5 154.0 437.7 154.0C437.9 154.0 438.0 167.7 438.0 184.3L438.0 214.7L404.2 231.0C385.7 240.0 366.1 249.5 360.6 252.2C355.2 254.8 350.3 257.0 349.9 257.0C349.4 257.0 326.9 244.4 299.9 229.0L250.7 201.0L225.6 215.3C211.8 223.2 187.8 236.8 172.3 245.6L144.1 261.5L144.0 284.8C144.0 297.5 143.7 308.0 143.2 307.9C142.8 307.9 127.4 299.6 109.0 289.4Z";

export function brandMark(size = 17): SVGElement {
  const el = document.createElementNS(SVG_NS, "svg");
  // The traced artwork does not fill its 512 box, so the viewBox is the artwork's own
  // bounding box and the rendered size is the mark's optical height.
  el.setAttribute("viewBox", "63 45 374 428");
  el.setAttribute("width", String(size));
  el.setAttribute("height", String(Math.round(size * 428 / 374)));
  el.setAttribute("fill", "none");
  el.setAttribute("aria-hidden", "true");
  el.innerHTML = raw(`<path d="${BRAND_MARK_PATH}" fill="#F97301" fill-rule="evenodd"/>`).html;
  return el;
}
