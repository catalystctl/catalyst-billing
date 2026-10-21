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

/** The wordmark's glyph: a C whose aperture is a ledger dot. */
export function brandMark(size = 17): SVGElement {
  const el = document.createElementNS(SVG_NS, "svg");
  el.setAttribute("viewBox", "0 0 32 32");
  el.setAttribute("width", String(size));
  el.setAttribute("height", String(size));
  el.setAttribute("fill", "none");
  el.setAttribute("aria-hidden", "true");
  el.innerHTML = raw(
    '<path d="M23.4 7.6a11 11 0 1 0 0 16.8" stroke="currentColor" stroke-width="3.4" stroke-linecap="round"/><circle cx="24.2" cy="16" r="3" fill="currentColor"/>',
  ).html;
  return el;
}
