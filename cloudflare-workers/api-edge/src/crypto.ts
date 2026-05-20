// AES-256-GCM helpers for secret-store envelope encryption.
//
// Wire format mirrors internal/crypto/encrypt.go: `nonce(12) || ciphertext`,
// stored as a BLOB in D1's secret_store_entries.encrypted_value. The same
// SECRET_ENCRYPTION_KEY (hex-encoded 32 bytes, lives in Infisical /shared/)
// is bound to the edge Worker as a secret AND distributed to every CP via
// the per-cell KV/SM sync — so a value encrypted here decrypts cleanly inside
// any cell, and vice versa.

const NONCE_LEN = 12;

async function importKey(hexKey: string): Promise<CryptoKey> {
  if (hexKey.length !== 64) {
    throw new Error(`SECRET_ENCRYPTION_KEY must be 64 hex chars (32 bytes), got ${hexKey.length}`);
  }
  const raw = new Uint8Array(32);
  for (let i = 0; i < 32; i++) {
    raw[i] = parseInt(hexKey.substr(i * 2, 2), 16);
  }
  return crypto.subtle.importKey("raw", raw, { name: "AES-GCM" }, false, ["encrypt", "decrypt"]);
}

export async function encryptSecret(hexKey: string, plaintext: string): Promise<Uint8Array> {
  const key = await importKey(hexKey);
  const nonce = new Uint8Array(NONCE_LEN);
  crypto.getRandomValues(nonce);
  const ct = new Uint8Array(
    await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce }, key, new TextEncoder().encode(plaintext)),
  );
  // nonce || ciphertext+tag
  const out = new Uint8Array(NONCE_LEN + ct.length);
  out.set(nonce, 0);
  out.set(ct, NONCE_LEN);
  return out;
}

export async function decryptSecret(hexKey: string, blob: Uint8Array): Promise<string> {
  if (blob.length < NONCE_LEN + 16) throw new Error("ciphertext too short");
  const key = await importKey(hexKey);
  const nonce = blob.subarray(0, NONCE_LEN);
  const ct = blob.subarray(NONCE_LEN);
  const pt = await crypto.subtle.decrypt({ name: "AES-GCM", iv: nonce }, key, ct);
  return new TextDecoder().decode(pt);
}
