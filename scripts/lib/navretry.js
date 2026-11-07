// navretry.js — the visual pass's navigation, retried ONCE on a classified transient (run 9, lane `retry`;
// scripts/e2e.d/README.md "Retry"). Required by visual.js, beside which it is mounted (/src/scripts/lib/).
//
// Gate 23 lost seven shots to `page.goto: net::ERR_CONNECTION_CLOSED at https://clippet.gg/` — the edge
// dropping connections under the pass's burst, not a page that was wrong. So a navigation that fails with
// one of the SAME Playwright signatures the E2E's classifier names (scripts/lib/transient.sh: pw-closed,
// pw-reset, pw-timeout) waits two seconds and goes once more, and says so on its own line:
//     [visual] retry <url> (<signature>)
// A second failure throws exactly as the first would have, so the shot fails as it always did. Any other
// error — an HTTP status the page answered, a Playwright timeout waiting for `networkidle`, an assertion —
// is never retried. `stats.retries` is what the pass's summary line counts.
"use strict";

const SIGNATURES = [
  [/net::ERR_CONNECTION_CLOSED/, "pw-closed"],
  [/net::ERR_CONNECTION_RESET/, "pw-reset"],
  [/net::ERR_(CONNECTION_)?TIMED_OUT/, "pw-timeout"],
];
const PAUSE_MS = 2000;
const stats = { retries: 0 };

// navSignature(err) → "pw-closed" | "pw-reset" | "pw-timeout" | "" (never retried)
function navSignature(err) {
  const text = String((err && err.message) || err || "");
  for (const [re, sig] of SIGNATURES) if (re.test(text)) return sig;
  return "";
}

// gotoRetry(page, url, opts, { log, redact, pauseMs }) — page.goto, once more on a classified failure.
async function gotoRetry(page, url, opts, { log = console.log, redact = (u) => u, pauseMs = PAUSE_MS } = {}) {
  try {
    return await page.goto(url, opts);
  } catch (e) {
    const sig = navSignature(e);
    if (!sig) throw e;
    stats.retries++;
    log(`[visual] retry ${redact(url)} (${sig})`);
    await new Promise((r) => setTimeout(r, pauseMs));
    return await page.goto(url, opts);
  }
}

module.exports = { navSignature, gotoRetry, stats, PAUSE_MS };
