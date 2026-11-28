# Single sign-on

This document explains how to configure OpenID Connect (OIDC) single sign-on in Corral, how identities, groups, and logout behave, and how to set up Keycloak as a provider.

Corral can authenticate users with any standard OpenID Connect provider (Keycloak, Authentik, Authelia, Microsoft Entra ID, Okta, Google, and others) alongside or instead of local passwords.

## Behavior

- **Protocol and verification.** Corral uses the Authorization Code flow with PKCE (`S256`), `state`, and `nonce`, acting as a confidential client. ID token signatures are verified against the provider's published JWKS keys (automatically refetched when an unknown key ID appears). Only asymmetric signing algorithms are accepted (never `none` or `HS*`). Issuer, audience (and authorized party), expiry, issue time, and nonce are all verified. Provider tokens are not persisted.
- **Identity and account linking.** A user is identified by the provider's issuer and subject (`sub`), never by email or username, which a provider may allow users to change. The first SSO sign-in creates a passwordless account whose username is derived from `preferred_username` (falling back to email); if that username is already taken, `-2`, `-3`, and so on is appended so an existing account is never taken over. Existing local accounts are never linked automatically by email: a signed-in user links their account manually in **Settings -> Account -> Single sign-on**, which keeps their password working. An account with no password cannot be unlinked from SSO (disable it instead, or set a password first).
- **Groups and roles.** When `CORRAL_OIDC_ALLOWED_GROUPS` is set, only members of those groups can sign in. When `CORRAL_OIDC_ADMIN_GROUPS` is set, group membership decides administrator status on every SSO sign-in (removing admin status if the user leaves those groups, except for the last remaining administrator). A disabled Corral account remains blocked regardless of provider state. Corral's own two-step sign-in (TOTP) is skipped for SSO logins because the provider handles MFA, while continuing to apply to password logins for the same account.
- **Signing out.** Corral supports OpenID Connect **back-channel logout**. Register `https://<your-corral>/api/auth/oidc/backchannel` at the provider; when a user signs out there or an administrator ends their session, Corral terminates the sessions created by SSO (leaving password-created sessions intact). With `CORRAL_OIDC_END_SESSION=true`, signing out of Corral also redirects to the provider's `end_session_endpoint` when available.
- **Multiple domains.** Each origin configured via `CORRAL_PUBLIC_URL` and `CORRAL_EXTRA_URLS` signs in on its own domain. Register `https://<each-origin>/api/auth/oidc/callback` as a valid redirect URI (and `https://<each-origin>/login` as a post-logout redirect URI if using end-session). Forwarded ports and VS Code continue to authenticate through `CORRAL_PUBLIC_URL`.
- **Keeping recovery access.** Setting `CORRAL_LOCAL_LOGIN=off` disables password sign-in for everyone, including administrators; if the provider is unreachable or misconfigured, no one can sign in. Setting `CORRAL_LOCAL_LOGIN=admins` keeps password sign-in available for administrators as a fallback.

## Environment variables

See [Configuration](configuration.md#single-sign-on) for the full table of `CORRAL_OIDC_*` and `CORRAL_LOCAL_LOGIN` variables, defaults, and validation rules.

## Example: Keycloak

1. In your Keycloak realm, go to **Clients -> Create client**: set client type to **OpenID Connect** and client ID to `corral`.
2. Under **Capability config**, enable **Client authentication** (confidential client) and **Standard flow**; disable **Direct access grants** and **Implicit flow**.
3. Under **Login settings**, configure:
   - **Valid redirect URIs**: `https://ssh.example.com/api/auth/oidc/callback`
   - **Valid post logout redirect URIs**: `https://ssh.example.com/login`
   - **Web origins**: `https://ssh.example.com`

   Add an entry for `CORRAL_PUBLIC_URL` and for every origin in `CORRAL_EXTRA_URLS`, since sign-in and sign-out return to the domain they began on.
4. Under **Advanced -> Advanced settings**, set **Proof Key for Code Exchange Code Challenge Method** to `S256`.
5. Under **Logout settings**, disable **Front channel logout**, set **Backchannel logout URL** to `https://ssh.example.com/api/auth/oidc/backchannel`, and enable **Backchannel logout session required**.
6. Under **Client scopes -> corral-dedicated -> Add mapper -> By configuration -> Group Membership**, create a mapper named `groups` with **Token Claim Name** `groups`, **Full group path** off, and **Add to ID token** on.
7. Under **Credentials**, copy the **Client secret**.

Configure Corral with:

```sh
CORRAL_OIDC_ISSUER=https://id.example.com/realms/example
CORRAL_OIDC_CLIENT_ID=corral
CORRAL_OIDC_CLIENT_SECRET_FILE=/run/secrets/corral-oidc
CORRAL_OIDC_ALLOWED_GROUPS=corral-users,corral-admins
CORRAL_OIDC_ADMIN_GROUPS=corral-admins
CORRAL_LOCAL_LOGIN=admins
```

If **Full group path** is enabled in Keycloak, group names arrive with a leading slash (for example `/corral-users`) and must be listed that way in `CORRAL_OIDC_ALLOWED_GROUPS` and `CORRAL_OIDC_ADMIN_GROUPS`.

Because Corral does not retain ID tokens, Keycloak displays a confirmation prompt before logging out when `CORRAL_OIDC_END_SESSION=true` is enabled.
