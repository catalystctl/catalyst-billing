// Session loading.

import { ApiError, type Api } from "./api";
import type { Session, SessionUser } from "./types";

interface MeResponse {
  user: SessionUser;
  permissions: string[] | null;
  csrf_token: string;
  expires_at: string;
  /** Set by an operator's MFA reset: the account owes the panel a new second factor. */
  mfa_enrolment_required?: boolean;
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
    };
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) return null;
    throw err;
  }
}

export function homeFor(session: Session): string {
  return session.user.is_staff ? "/" : "/portal";
}
