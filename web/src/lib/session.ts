// Session loading.

import { ApiError, type Api } from "./api";
import type { Session, SessionUser } from "./types";

interface MeResponse {
  user: SessionUser;
  permissions: string[];
  csrf_token: string;
  expires_at: string;
}

/** Reads the current identity, or null when nobody is signed in. */
export async function loadSession(api: Api): Promise<Session | null> {
  try {
    const me = await api.get<MeResponse>("/api/v1/auth/me", { cache: false });
    api.setCSRFToken(me.csrf_token);
    return {
      user: me.user,
      permissions: new Set(me.permissions),
      csrfToken: me.csrf_token,
      expiresAt: me.expires_at,
    };
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) return null;
    throw err;
  }
}

export function homeFor(session: Session): string {
  return session.user.is_staff ? "/" : "/portal";
}
