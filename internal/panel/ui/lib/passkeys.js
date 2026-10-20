// Passkeys in the browser: the server sends options with base64url
// strings, navigator.credentials wants ArrayBuffers, and the answer goes
// back as base64url again. Nothing else happens here.

import { api } from "./api.js";

const toBuf = (s) => {
  const b = atob(s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((s.length + 3) % 4));
  return Uint8Array.from(b, (c) => c.charCodeAt(0)).buffer;
};
const toB64 = (buf) => btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

// supported is whether this browser, on this origin, can use passkeys at
// all: they need a secure origin with a name, never a bare IP.
export function supported() {
  return Boolean(window.PublicKeyCredential && window.isSecureContext && !/^[\d.]+$|^\[/.test(location.hostname));
}

export async function register(name) {
  const o = await api.post("/auth/passkeys/register/options");
  const pk = o.publicKey;
  const cred = await navigator.credentials.create({
    publicKey: {
      ...pk,
      challenge: toBuf(pk.challenge),
      user: { ...pk.user, id: toBuf(pk.user.id) },
      excludeCredentials: (pk.excludeCredentials || []).map((c) => ({ ...c, id: toBuf(c.id) })),
    },
  });
  return api.post("/auth/passkeys/register", {
    challenge_id: o.challenge_id, name,
    client_data_json: toB64(cred.response.clientDataJSON),
    attestation_object: toB64(cred.response.attestationObject),
  });
}

export async function login() {
  const o = await api.post("/auth/passkeys/login/options", {}, { allow401: true });
  const pk = o.publicKey;
  const cred = await navigator.credentials.get({
    publicKey: { ...pk, challenge: toBuf(pk.challenge), allowCredentials: (pk.allowCredentials || []).map((c) => ({ ...c, id: toBuf(c.id) })) },
  });
  return api.post("/auth/passkeys/login", {
    challenge_id: o.challenge_id, id: toB64(cred.rawId),
    client_data_json: toB64(cred.response.clientDataJSON),
    authenticator_data: toB64(cred.response.authenticatorData),
    signature: toB64(cred.response.signature),
  }, { allow401: true });
}
