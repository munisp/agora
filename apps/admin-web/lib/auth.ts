import NextAuth from "next-auth";
import Keycloak from "next-auth/providers/keycloak";
import type { NextAuthConfig } from "next-auth";

/**
 * Auth.js v5 configuration for Keycloak OIDC (Authorization Code + PKCE).
 *
 * The Keycloak `admin-web` client is a *public* client, so no client secret is
 * required; Auth.js automatically uses PKCE (S256) when no secret is set.
 *
 * KEYCLOAK_ISSUER must be reachable from this server (token exchange, JWKS,
 * discovery). When the browser cannot reach that same URL (e.g. the issuer is
 * the in-cluster `http://keycloak:8080`), set KEYCLOAK_ISSUER_PUBLIC to the
 * browser-reachable equivalent and it will be used for the authorize redirect.
 */

declare module "next-auth" {
  interface Session {
    /** Keycloak access token, forwarded to the APISIX gateway by the BFF. */
    accessToken?: string;
    /** Tenant slugs from the `tenant_slugs` claim (Keycloak group mapper). */
    tenantSlugs: string[];
    /** Keycloak realm roles from the `realm_access.roles` claim. */
    realmRoles: string[];
    error?: string;
  }
}

declare module "@auth/core/jwt" {
  interface JWT {
    access_token?: string;
    refresh_token?: string;
    expires_at?: number;
    tenant_slugs?: string[];
    realm_roles?: string[];
    error?: string;
  }
}

const issuer = process.env.KEYCLOAK_ISSUER ?? "http://keycloak:8080/realms/opendesk";
const publicIssuer = process.env.KEYCLOAK_ISSUER_PUBLIC;
const clientId = process.env.KEYCLOAK_CLIENT_ID ?? "admin-web";
const clientSecret = process.env.KEYCLOAK_CLIENT_SECRET || undefined;

/** Decode the payload of a JWT without verifying (claims already trusted from Keycloak). */
function decodeClaims(token: string): Record<string, unknown> {
  try {
    const payload = token.split(".")[1];
    return JSON.parse(Buffer.from(payload, "base64url").toString("utf8"));
  } catch {
    return {};
  }
}

/** Extract Keycloak realm roles from a decoded access-token payload. */
function realmRolesFromClaims(claims: Record<string, unknown>): string[] {
  const realmAccess = claims.realm_access;
  if (typeof realmAccess === "object" && realmAccess !== null) {
    const roles = (realmAccess as { roles?: unknown }).roles;
    if (Array.isArray(roles)) return roles.filter((r): r is string => typeof r === "string");
  }
  return [];
}

async function refreshAccessToken(token: {
  refresh_token?: string;
  [key: string]: unknown;
}) {
  try {
    const res = await fetch(`${issuer}/protocol/openid-connect/token`, {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({
        grant_type: "refresh_token",
        client_id: clientId,
        refresh_token: token.refresh_token ?? "",
      }),
    });
    const refreshed = await res.json();
    if (!res.ok) throw refreshed;
    return {
      ...token,
      access_token: refreshed.access_token,
      expires_at: Math.floor(Date.now() / 1000) + (refreshed.expires_in ?? 300),
      refresh_token: refreshed.refresh_token ?? token.refresh_token,
      // Roles and business memberships can change between refreshes; re-read both from the new token
      // (a business someone was added to appeared only after signing out and in again).
      realm_roles: realmRolesFromClaims(decodeClaims(refreshed.access_token)),
      ...(Array.isArray(decodeClaims(refreshed.access_token).tenant_slugs)
        ? { tenant_slugs: decodeClaims(refreshed.access_token).tenant_slugs as string[] }
        : {}),
      error: undefined,
    };
  } catch {
    return { ...token, error: "RefreshAccessTokenError" };
  }
}

export const authConfig = {
  providers: [
    Keycloak({
      clientId,
      clientSecret,
      issuer,
      // Public client => PKCE is enforced by Auth.js automatically.
      checks: ["pkce", "state"],
      ...(publicIssuer && publicIssuer !== issuer
        ? {
            authorization: {
              url: `${publicIssuer}/protocol/openid-connect/auth`,
              params: { scope: "openid profile email" },
            },
          }
        : {}),
    }),
  ],
  session: { strategy: "jwt" },
  pages: { signIn: "/sign-in" },
  callbacks: {
    async jwt({ token, account }) {
      // Initial sign-in: persist tokens + tenant_slugs claim.
      if (account?.access_token) {
        const claims = decodeClaims(account.access_token);
        const slugs = claims.tenant_slugs;
        return {
          ...token,
          access_token: account.access_token,
          refresh_token: account.refresh_token,
          expires_at:
            account.expires_at ?? Math.floor(Date.now() / 1000) + 300,
          tenant_slugs: Array.isArray(slugs) ? (slugs as string[]) : [],
          realm_roles: realmRolesFromClaims(claims),
          error: undefined,
        };
      }
      // Token still valid.
      if (token.expires_at && Date.now() < (token.expires_at - 30) * 1000) {
        return token;
      }
      // Expired: refresh.
      return refreshAccessToken(token);
    },
    async session({ session, token }) {
      session.accessToken = token.access_token;
      session.tenantSlugs = token.tenant_slugs ?? [];
      session.realmRoles = token.realm_roles ?? [];
      session.error = token.error;
      return session;
    },
  },
} satisfies NextAuthConfig;

export const { handlers, auth, signIn, signOut } = NextAuth(authConfig);
