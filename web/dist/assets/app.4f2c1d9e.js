// The application shell's script.
//
// It does one thing: ask the API who the caller is and report it. That is enough to
// prove three properties the serving layer is responsible for — the SPA loads, the
// session cookie is sent to /api/v1, and a JSON error is rendered rather than swallowed.
//
// It is a module and it contains no inline event handlers, so the Content-Security-Policy
// needs no exemption for it.

const status = document.getElementById("status");

function render(text, tone) {
  if (!status) return;
  status.textContent = text;
  status.dataset.tone = tone;
}

async function main() {
  try {
    const res = await fetch("/api/v1/auth/me", {
      headers: { Accept: "application/json" },
      credentials: "same-origin",
    });
    if (res.status === 401) {
      render("Not signed in. Sign in with POST /api/v1/auth/login.", "info");
      return;
    }
    if (!res.ok) {
      const problem = await res.json().catch(() => ({}));
      render(`Request failed: ${problem.code || res.status}`, "error");
      return;
    }
    const body = await res.json();
    const who = body.user || {};
    render(`Signed in as ${who.email || who.id}.`, "ok");
  } catch (err) {
    render(`Could not reach the API: ${err.message}`, "error");
  }
}

main();
