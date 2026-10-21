// Theme.
//
// Applied on <html data-theme> so every token switches at once and no component needs to
// know which theme it is in. The stored choice wins over the OS preference; a first visit
// follows the OS. The attribute is set before the first paint (main.ts runs as a module and
// the shell's background is on <body>), so there is no flash of the wrong theme.

export type Theme = "dark" | "light";

const KEY = "catalyst.theme";

export function initTheme(): Theme {
  let theme: Theme | null = null;
  try {
    const stored = localStorage.getItem(KEY);
    if (stored === "dark" || stored === "light") theme = stored;
  } catch {
    // A blocked storage API is not a reason to fail to render.
  }
  if (!theme) {
    theme = window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  }
  document.documentElement.dataset.theme = theme;
  return theme;
}

export function applyTheme(theme: Theme): void {
  document.documentElement.dataset.theme = theme;
  try {
    localStorage.setItem(KEY, theme);
  } catch {
    /* ignore */
  }
}

export function toggleTheme(current: Theme): Theme {
  const next: Theme = current === "dark" ? "light" : "dark";
  applyTheme(next);
  return next;
}
