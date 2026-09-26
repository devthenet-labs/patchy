// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { test } from "node:test";
import { readProvider, readAuthError, consumeLogoutMarker } from "../src/auth.ts";

// A narrow document.cookie double: model prefix rejection on writes so a
// deletion without Secure or with the wrong Path cannot appear to succeed.
function browser(protocol, cookies) {
  const jar = new Map(Object.entries(cookies));
  const writes = [];
  globalThis.location = { protocol };
  globalThis.document = {
    get cookie() {
      return [...jar].map(([name, value]) => `${name}=${value}`).join("; ");
    },
    set cookie(value) {
      writes.push(value);
      const [pair, ...attributes] = value.split(";").map((s) => s.trim());
      const [name] = pair.split("=");
      const attrs = attributes.map((s) => s.toLowerCase());
      if (name.startsWith("__Host-") &&
          (protocol !== "https:" || !attrs.includes("secure") ||
           !attrs.includes("path=/") || attrs.some((s) => s.startsWith("domain=")))) return;
      if (attrs.includes("max-age=0")) jar.delete(name);
    },
  };
  return { jar, writes };
}

const encode = (value) => Buffer.from(JSON.stringify(value)).toString("base64url");

test("HTTPS reads only host-bound cookies, ignoring legacy and dev cookies", () => {
  browser("https:", {
    "patchy-auth-provider": encode({ provider: "forged" }),
    "patchy-dev-auth-provider": encode({ provider: "dev" }),
    "patchy-auth-error": encode("forged error"),
    "patchy-dev-auth-error": encode("dev error"),
    "patchy-auth-logout": "true",
    "patchy-dev-auth-logout": "true",
  });
  assert.equal(readProvider(), null);
  assert.equal(readAuthError(), null);
  assert.equal(consumeLogoutMarker(), false);
});

test("HTTPS consumes host-bound error and logout cookies exactly once", () => {
  const { jar, writes } = browser("https:", {
    "__Host-patchy-auth-provider": encode({ provider: "oidc", authenticated: true }),
    "__Host-patchy-auth-error": encode("sign-in failed"),
    "__Host-patchy-auth-logout": "true",
  });
  assert.deepEqual(readProvider(), { provider: "oidc", authenticated: true });
  assert.equal(readAuthError(), "sign-in failed");
  assert.equal(readAuthError(), null);
  assert.equal(consumeLogoutMarker(), true);
  assert.equal(consumeLogoutMarker(), false);
  assert.equal(jar.size, 1);
  assert.equal(writes.length, 2);
  assert.ok(writes.every((s) => /;\s*secure(?:;|$)/i.test(s)));
  assert.ok(writes.every((s) => !/domain=/i.test(s)));
});

test("HTTP local development uses only its separate cookie namespace", () => {
  const { writes } = browser("http:", {
    "patchy-dev-auth-provider": encode({ provider: "oidc", authenticated: false }),
    "patchy-dev-auth-error": encode("dev failure"),
    "patchy-dev-auth-logout": "true",
    "__Host-patchy-auth-provider": encode({ provider: "wrong" }),
  });
  assert.deepEqual(readProvider(), { provider: "oidc", authenticated: false });
  assert.equal(readAuthError(), "dev failure");
  assert.equal(readAuthError(), null);
  assert.equal(consumeLogoutMarker(), true);
  assert.equal(consumeLogoutMarker(), false);
  assert.ok(writes.every((s) => s.startsWith("patchy-dev-")));
});
