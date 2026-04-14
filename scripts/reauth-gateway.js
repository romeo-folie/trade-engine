#!/usr/bin/env node
/**
 * reauth-gateway.js
 *
 * Automates re-authentication of the IBKR Client Portal Gateway after the
 * nightly maintenance window. Intended to run via cron at 06:00 UTC Mon–Fri,
 * ~15 minutes after the maintenance window closes and ~90 minutes before the
 * London session engine start at 07:30 UTC.
 *
 * Required env vars:
 *   IBKR_API_USERNAME     — gateway login username
 *   IBKR_API_PASSWORD     — gateway login password
 *
 * Optional env vars:
 *   IBKR_GATEWAY_URL      — defaults to https://localhost:5001
 *   TELEGRAM_BOT_TOKEN    — send result notifications (skipped if unset)
 *   TELEGRAM_CHAT_ID      — required when TELEGRAM_BOT_TOKEN is set
 *
 * Exit codes:
 *   0 — authenticated successfully (or already authenticated)
 *   1 — authentication failed after all retries
 */

const { chromium } = require('playwright');
const https = require('https');

// ── Config ────────────────────────────────────────────────────────────────────

const GATEWAY_URL  = process.env.IBKR_GATEWAY_URL || 'https://localhost:5001';
const USERNAME     = process.env.IBKR_API_USERNAME;
const PASSWORD     = process.env.IBKR_API_PASSWORD;
const TG_TOKEN     = process.env.TELEGRAM_BOT_TOKEN;
const TG_CHAT_ID   = process.env.TELEGRAM_CHAT_ID;

// Retry the full login sequence up to MAX_ATTEMPTS times with RETRY_DELAY_MS
// between attempts. The gateway can be slow to accept new sessions immediately
// after the maintenance window.
const MAX_ATTEMPTS   = 3;
const RETRY_DELAY_MS = 30_000;

// How long to wait for the login page to load and for the post-login auth
// status check before giving up on a single attempt.
const NAV_TIMEOUT_MS = 30_000;
const AUTH_POLL_MS   = 3_000;
const AUTH_MAX_POLLS = 10;

// Timeout for outbound HTTP requests (Telegram send, auth status check).
const HTTP_TIMEOUT_MS = 10_000;

// Only disable TLS verification when the gateway host is localhost / loopback.
// Parsed inside a helper so that an invalid IBKR_GATEWAY_URL does not crash
// the module before main() can report the error; defaults to secure (true) on
// any parse failure.
function getRejectUnauthorized(gatewayUrl) {
  try {
    const { URL } = require('url');
    const host = new URL(gatewayUrl).hostname;
    return host !== 'localhost' && host !== '127.0.0.1';
  } catch (e) {
    console.warn(`Invalid IBKR_GATEWAY_URL "${gatewayUrl}": ${e.message}. Defaulting to TLS verification enabled.`);
    return true;
  }
}

const rejectUnauthorized = getRejectUnauthorized(GATEWAY_URL);
const isLocalhost        = !rejectUnauthorized;

// ── Telegram helper ───────────────────────────────────────────────────────────

// Escape characters that are special in Telegram's HTML parse mode so that
// dynamic content (error messages, URLs) never causes a 400 from the API.
function escapeTelegramHtml(text) {
  return String(text)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;');
}

async function sendTelegram(text) {
  if (!TG_TOKEN || !TG_CHAT_ID) return;
  const body = JSON.stringify({ chat_id: TG_CHAT_ID, text: escapeTelegramHtml(text), parse_mode: 'HTML' });
  return new Promise((resolve) => {
    const req = https.request(
      {
        hostname: 'api.telegram.org',
        path: `/bot${TG_TOKEN}/sendMessage`,
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) },
      },
      (res) => {
        res.resume();
        res.on('end', resolve);
      }
    );
    req.setTimeout(HTTP_TIMEOUT_MS, () => {
      req.destroy(new Error('Telegram request timed out'));
    });
    req.on('error', (e) => { console.warn('Telegram send failed:', e.message); resolve(); });
    req.write(body);
    req.end();
  });
}

// ── Auth status check ─────────────────────────────────────────────────────────

// checkAuthStatusDirect: plain HTTPS request, no session cookies.
// Works before any browser session exists (pre-login fast-path).
// Returns false on 403/401 — the gateway requires cookies once a session exists.
function checkAuthStatusDirect() {
  return new Promise((resolve) => {
    const req = https.request(
      `${GATEWAY_URL}/v1/api/iserver/auth/status`,
      { rejectUnauthorized },
      (res) => {
        let data = '';
        res.on('data', (chunk) => { data += chunk; });
        res.on('end', () => {
          try {
            const json = JSON.parse(data);
            resolve(json.authenticated === true && json.connected === true);
          } catch {
            resolve(false);
          }
        });
      }
    );
    req.setTimeout(HTTP_TIMEOUT_MS, () => {
      req.destroy(new Error('Auth status check timed out'));
    });
    req.on('error', () => resolve(false));
    req.end();
  });
}

// checkAuthStatusFromPage: fetch via the Playwright page so the browser's
// session cookies are included. Required after login because the gateway now
// scopes the authenticated session to the cookie set at login time.
async function checkAuthStatusFromPage(page) {
  try {
    return await page.evaluate(async () => {
      const r = await fetch('/v1/api/iserver/auth/status');
      if (!r.ok) return false;
      const j = await r.json();
      return j.authenticated === true && j.connected === true;
    });
  } catch {
    return false;
  }
}

// ── Login via browser ─────────────────────────────────────────────────────────

async function attemptLogin(browser) {
  const context = await browser.newContext({ ignoreHTTPSErrors: isLocalhost });
  const page    = await context.newPage();

  try {
    console.log(`  Navigating to ${GATEWAY_URL} ...`);
    await page.goto(GATEWAY_URL, { waitUntil: 'domcontentloaded', timeout: NAV_TIMEOUT_MS });

    // Wait for the username field — the gateway redirects to /sso/Login which
    // renders the form dynamically. Field IDs changed in the 2024+ login UI.
    await page.waitForSelector('#xyz-field-username', { timeout: NAV_TIMEOUT_MS });

    // Switch to Paper mode if the toggle is not already set. The toggle
    // defaults to Live; paper trading accounts must select Paper or login fails.
    // The actual <input> is hidden — click the visible <label for="toggle1"> instead.
    // Wait after clicking for the form to re-render before filling credentials.
    const paperToggle = page.locator('input[name="paperSwitch"]');
    const isPaper = await paperToggle.isChecked().catch(() => false);
    if (!isPaper) {
      console.log('  Switching to Paper trading mode...');
      await page.locator('label[for="toggle1"]').click();
      await new Promise(r => setTimeout(r, 1000));
    }

    console.log('  Filling credentials...');
    // Use keyboard.type() rather than fill() — the gateway's JS framework
    // requires real keystroke events to bind the input values to its model.
    await page.locator('#xyz-field-username').click();
    await page.keyboard.type(USERNAME);
    await page.locator('#xyz-field-password').click();
    await page.keyboard.type(PASSWORD);

    // Login button in the current gateway UI.
    const submitSelector = 'button.xyz-button-login[type="submit"]';
    await page.click(submitSelector);

    // After clicking, the gateway shows a brief loading state then either:
    //   a) lands on the authenticated home page, or
    //   b) shows a "paper account" confirmation dialog that needs a second click.
    // We poll the auth/status endpoint rather than relying on page selectors so
    // this stays resilient to gateway UI changes.
    console.log('  Waiting for authentication to confirm...');
    for (let i = 0; i < AUTH_MAX_POLLS; i++) {
      await new Promise(r => setTimeout(r, AUTH_POLL_MS));

      // Handle optional "paper trading" confirmation dialog.
      const paperBtn = page.locator('button:has-text("Continue"), button:has-text("Confirm"), #paperYes');
      if (await paperBtn.first().isVisible({ timeout: 500 }).catch(() => false)) {
        console.log('  Dismissing paper account confirmation dialog...');
        await paperBtn.first().click();
      }

      const authed = await checkAuthStatusFromPage(page);
      if (authed) {
        console.log(`  Authenticated after ${(i + 1) * AUTH_POLL_MS / 1000}s.`);
        return true;
      }
    }

    console.warn('  Auth status never confirmed within poll window.');
    return false;
  } finally {
    await context.close();
  }
}

// ── Main ──────────────────────────────────────────────────────────────────────

async function main() {
  if (!USERNAME || !PASSWORD) {
    console.error('IBKR_API_USERNAME and IBKR_API_PASSWORD must be set.');
    await sendTelegram('⚠️ IBKR reauth skipped — credentials not configured (IBKR_API_USERNAME / IBKR_API_PASSWORD missing).');
    return 1;
  }

  // Fast-path: already authenticated (e.g. reauth ran twice, or session survived).
  // Uses direct HTTP; returns false if the gateway requires cookies (post-login),
  // in which case we fall through to the browser login which is safe to repeat.
  if (await checkAuthStatusDirect()) {
    console.log('Gateway already authenticated — nothing to do.');
    await sendTelegram('✅ IBKR gateway already authenticated — no reauth needed.');
    return 0;
  }

  const browser = await chromium.launch({ headless: true });

  try {
    for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt++) {
      console.log(`\nAttempt ${attempt}/${MAX_ATTEMPTS}`);
      const ok = await attemptLogin(browser).catch((err) => {
        console.warn('  Login attempt threw:', err.message);
        return false;
      });

      if (ok) {
        console.log('\n✓ IBKR gateway re-authenticated successfully.');
        await sendTelegram('✅ IBKR gateway re-authenticated — engine ready for next session.');
        return 0;
      }

      if (attempt < MAX_ATTEMPTS) {
        console.log(`  Retrying in ${RETRY_DELAY_MS / 1000}s...`);
        await new Promise(r => setTimeout(r, RETRY_DELAY_MS));
      }
    }

    console.error('\n✗ IBKR re-authentication failed after all attempts.');
    await sendTelegram(
      `🔐 IBKR re-authentication failed after ${MAX_ATTEMPTS} attempts.\n\n` +
      `Manual login required: ${GATEWAY_URL}\n` +
      `The engine will not start for the next session until this is resolved.`
    );
    return 1;

  } finally {
    // browser.close() always runs here — process.exitCode is set after this
    // block completes so the finally is never bypassed.
    await browser.close();
  }
}

main()
  .then((code) => { process.exitCode = code; })
  .catch(async (err) => {
    console.error('Unexpected error:', err);
    await sendTelegram(`🔐 IBKR reauth script crashed: ${err.message}`);
    process.exitCode = 1;
  });
