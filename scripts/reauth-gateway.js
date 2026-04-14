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
const { URL }  = require('url');

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
// This prevents silent MITM if IBKR_GATEWAY_URL is ever pointed elsewhere.
const gatewayHost        = new URL(GATEWAY_URL).hostname;
const isLocalhost        = gatewayHost === 'localhost' || gatewayHost === '127.0.0.1';
const rejectUnauthorized = !isLocalhost;

// ── Telegram helper ───────────────────────────────────────────────────────────

async function sendTelegram(text) {
  if (!TG_TOKEN || !TG_CHAT_ID) return;
  const body = JSON.stringify({ chat_id: TG_CHAT_ID, text, parse_mode: 'HTML' });
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

// ── Auth status check (direct HTTP, no browser) ───────────────────────────────

function checkAuthStatus() {
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

// ── Login via browser ─────────────────────────────────────────────────────────

async function attemptLogin(browser) {
  const context = await browser.newContext({ ignoreHTTPSErrors: isLocalhost });
  const page    = await context.newPage();

  try {
    console.log(`  Navigating to ${GATEWAY_URL} ...`);
    await page.goto(GATEWAY_URL, { waitUntil: 'domcontentloaded', timeout: NAV_TIMEOUT_MS });

    // Wait for the username field — the gateway may redirect to /sso/Login first.
    await page.waitForSelector('#user_name', { timeout: NAV_TIMEOUT_MS });

    console.log('  Filling credentials...');
    await page.fill('#user_name', USERNAME);
    await page.fill('#password', PASSWORD);

    // The submit button is either a button or an input depending on gateway version.
    const submitSelector = 'button#submitForm, input#submitForm, button[type="submit"]';
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

      const authed = await checkAuthStatus();
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
  if (await checkAuthStatus()) {
    console.log('Gateway already authenticated — nothing to do.');
    await sendTelegram('✅ IBKR gateway already authenticated — no reauth needed.');
    return 0;
  }

  const browser = await chromium.launch({ headless: true });

  try {
    for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt++) {
      console.log(`\nAttempt ${attempt}/${MAX_ATTEMPTS}`);
      const ok = await attemptLogin(browser);

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
