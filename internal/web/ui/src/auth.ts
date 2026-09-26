// Sign-in surface helpers. The server tells the SPA how (and whether)
// sign-in works through three small SPA-readable cookies; the session itself
// lives in HttpOnly cookies the client never touches:
//
//   __Host-patchy-auth-provider — {provider, authenticated, autoLogin}
//                                (base64url JSON). Absent means unconfigured.
//   __Host-patchy-auth-error    — human-readable sign-in failure, shown once.
//   __Host-patchy-auth-logout   — sign-out marker; suppresses autoLogin.
// HTTP local development uses patchy-dev-* instead. HTTPS never falls back
// to dev or legacy cookies: a sibling preview could inject those names.

export interface AuthProvider {
  provider: string;
  authenticated: boolean;
  autoLogin?: boolean;
}

const PROVIDER_COOKIE = "auth-provider";
const ERROR_COOKIE = "auth-error";
const LOGOUT_COOKIE = "auth-logout";

function cookieName(name: string): string {
  return (location.protocol === "https:" ? "__Host-patchy-" : "patchy-dev-") + name;
}

function readCookie(name: string): string | null {
  for (const part of document.cookie.split(";")) {
    const [key, ...rest] = part.trim().split("=");
    if (key === cookieName(name) && rest.length) return rest.join("=");
  }
  return null;
}

function readJSONCookie<T>(name: string): T | null {
  const raw = readCookie(name);
  if (!raw) return null;
  try {
    return JSON.parse(atob(raw.replace(/-/g, "+").replace(/_/g, "/"))) as T;
  } catch {
    return null;
  }
}

function deleteCookie(name: string): void {
  const secure = location.protocol === "https:" ? "; Secure" : "";
  document.cookie = `${cookieName(name)}=; Path=/; Max-Age=0; SameSite=Lax${secure}`;
}

// readProvider returns the sign-in surface descriptor, or null when the
// server has no authentication configured (rollups-only posture).
export function readProvider(): AuthProvider | null {
  const p = readJSONCookie<AuthProvider>(PROVIDER_COOKIE);
  return p && typeof p.provider === "string" ? p : null;
}

// readAuthError returns and clears the last sign-in failure.
export function readAuthError(): string | null {
  const msg = readJSONCookie<string>(ERROR_COOKIE);
  if (msg) deleteCookie(ERROR_COOKIE);
  return typeof msg === "string" && msg ? msg : null;
}

// consumeLogoutMarker reports (and clears) an explicit sign-out, so
// autoLogin pauses for one visit instead of bouncing straight back.
export function consumeLogoutMarker(): boolean {
  const present = readCookie(LOGOUT_COOKIE) !== null;
  if (present) deleteCookie(LOGOUT_COOKIE);
  return present;
}

// signInURL starts the server-side sign-in flow, returning here afterwards.
export function signInURL(): string {
  const here = location.pathname + location.search + location.hash;
  return `/oauth2/authorize?originalPath=${encodeURIComponent(here)}`;
}

// signOut posts the logout (POST-only, CSRF hardening) and reloads.
export async function signOut(): Promise<void> {
  try {
    await fetch("/logout", { method: "POST" });
  } finally {
    location.href = "/";
  }
}
