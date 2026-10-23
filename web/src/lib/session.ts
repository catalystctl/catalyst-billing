// Session loading.

import { ApiError, type Api } from "./api";
import type { MfaState, Session, SessionUser } from "./types";

interface MeResponse {
  user: SessionUser;
  permissions: string[] | null;
  csrf_token: string;
  expires_at: string;
  /** Set by an operator's MFA reset: the account owes the panel a new second factor. */
  mfa_enrolment_required?: boolean;
  /** The same facts, in full. Absent on a server built before the field existed. */
  mfa?: Partial<MfaState> | null;
}

/** Reads the current identity, or null when nobody is signed in. */
export async function loadSession(api: Api): Promise<Session | null> {
  try {
    const me = await api.get<MeResponse>("/api/v1/auth/me", { cache: false });
    api.setCSRFToken(me.csrf_token);
    return {
      // The server marshals an absent list as null, not [], for a customer with no staff
      // roles. Normalising here means no page has to remember that `roles` can be null —
      // and one that forgets crashes the shell before the first screen renders.
      user: { ...me.user, roles: me.user.roles ?? [] },
      permissions: new Set(me.permissions ?? []),
      csrfToken: me.csrf_token,
      expiresAt: me.expires_at,
      // The server may omit the field on a build without the route; absent means "nothing owed".
      mfaEnrolmentRequired: me.mfa_enrolment_required === true,
      // Defaulted rather than trusted: a screen that reads `session.mfa.has_mfa` on a server that
      // does not report it would throw before rendering, and "off, and we will ask" is a better
      // first paint than a crash. The account screens re-read the real state from
      // `GET /api/v1/auth/mfa` on mount, so this default only ever lasts until that request lands.
      mfa: {
        has_mfa: me.mfa?.has_mfa === true,
        enrolled_at: me.mfa?.enrolled_at ?? null,
        recovery_codes_remaining: me.mfa?.recovery_codes_remaining ?? 0,
        // Two carriers of the same demand: the flat `mfa_enrolment_required` an operator's reset
        // sets, and the same fact inside `mfa`. Either being true means the account owes one, and
        // a build that reports only one of them still gets the redirect.
        enrolment_required: me.mfa?.enrolment_required === true || me.mfa_enrolment_required === true,
        reset_at: me.mfa?.reset_at ?? null,
        reset_by: me.mfa?.reset_by ?? null,
        available: me.mfa?.available !== false,
      },
    };
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) return null;
    throw err;
  }
}

/** Where a session belongs after signing in, or when a screen sends it away.
 *
 *  Staff land on `/dashboard`, not `/`: `/` is the storefront and is public, so a signed-in
 *  operator who is bounced to "home" was previously bounced to a page that had no URL of its
 *  own for them. `/dashboard` is a real, linkable address, which also means the dashboard can be
 *  bookmarked and pasted into a ticket like every other screen. */
export function homeFor(session: Session): string {
  return session.user.is_staff ? "/dashboard" : "/portal";
}
