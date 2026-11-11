// loadtest/k6/booking-public.js — k6 load test for the booking-service
// PUBLIC (anonymous) legs through the APISIX gateway (SPEC-W48-D, recon F8).
//
// Covered legs:
//   1. GET  /api/bookings/public/sites/{slug}/availability?offering_id=&date=
//   2. POST /api/bookings/v1/waitlist/claim            (token capability)
//   3. POST /api/bookings/public/sites/{slug}/bookings/{id}/pay   (W48-A A3)
//   4. GET  /api/bookings/public/sites/{slug}/reviews             (W48-A A4)
//
// MOCKING PAYMENTS IS FORBIDDEN: the pay leg runs against the REAL W48-A
// endpoint and accepts the full contract response set (200 with
// {link, tx_ref}; 400/404/409 validation/conflict; 503 when the Flutterwave
// rail is unconfigured in dev — see payments-service src/flutterwave.rs).
// The script asserts response SHAPE + latency budgets only, never a
// successful charge.
//
// Latency thresholds come from docs/performance-budgets.md: B1-B4 customer
// API budget p50 <= 300 ms, p99 <= 1.5 s (SLO 1 decomposition). The
// waitlist-claim leg rides the same public budget.
//
// Config (env):
//   BASE_URL    gateway origin            (default http://localhost:9080)
//   SITE_SLUG   published site slug       (default "demo")
//   BOOKING_ID  booking id for pay leg    (default nil-UUID -> expect 404)
//   DURATION    per-scenario duration     (default 30s)
//   VUS         virtual users per scenario(default 5)
//
// k6 JS (goja) — plain ES5.1-compatible JavaScript, no TypeScript syntax.

import http from 'k6/http';
import { check } from 'k6';

var BASE_URL = __ENV.BASE_URL || 'http://localhost:9080';
var SITE_SLUG = __ENV.SITE_SLUG || 'demo';
var BOOKING_ID = __ENV.BOOKING_ID || '00000000-0000-0000-0000-000000000000';
var DURATION = __ENV.DURATION || '30s';
var VUS = parseInt(__ENV.VUS || '5', 10);

// Budgets: docs/performance-budgets.md B1-B4 (p50 <= 300 ms, p99 <= 1.5 s).
var BUDGETS = ['p(50)<300', 'p(99)<1500'];

export var options = {
  scenarios: {
    availability: {
      executor: 'constant-vus', vus: VUS, duration: DURATION,
      exec: 'availability',
    },
    waitlist_claim: {
      executor: 'constant-vus', vus: Math.max(1, Math.floor(VUS / 2)),
      duration: DURATION, exec: 'waitlistClaim',
    },
    pay_leg: {
      executor: 'constant-vus', vus: Math.max(1, Math.floor(VUS / 2)),
      duration: DURATION, exec: 'payLeg',
    },
    reviews: {
      executor: 'constant-vus', vus: VUS, duration: DURATION,
      exec: 'reviews',
    },
  },
  thresholds: {
    // Per-leg latency budgets (tag submetrics).
    'http_req_duration{leg:availability}': BUDGETS,
    'http_req_duration{leg:waitlist_claim}': BUDGETS,
    'http_req_duration{leg:pay}': BUDGETS,
    'http_req_duration{leg:reviews}': BUDGETS,
    // With per-request expectedStatuses below, a "failed" request is an
    // UNEXPECTED status — tolerate almost none.
    http_req_failed: ['rate<0.05'],
    checks: ['rate>0.95'],
  },
};

function jsonHeaders() {
  return { 'Content-Type': 'application/json' };
}

// setup(): discover a real offering for the availability leg. The aggregate
// public availability variant requires offering_id + date (YYYY-MM-DD)
// (booking-service internal/httpapi/public.go publicAvailability).
export function setup() {
  var res = http.get(
    BASE_URL + '/api/bookings/public/sites/' + SITE_SLUG + '/offerings',
    { tags: { leg: 'setup' }, responseCallback: http.expectedStatuses(200, 404) }
  );
  var offeringId = '';
  try {
    var body = JSON.parse(res.body);
    var list = body.offerings || body.data || (Array.isArray(body) ? body : []);
    if (list.length > 0) {
      offeringId = list[0].id || list[0].offering_id || '';
    }
  } catch (e) {
    // Site not published / no offerings: availability falls back to the
    // site-shell GET, which is still a valid public leg.
  }
  var tomorrow = new Date(Date.now() + 24 * 3600 * 1000);
  var mm = ('0' + (tomorrow.getUTCMonth() + 1)).slice(-2);
  var dd = ('0' + tomorrow.getUTCDate()).slice(-2);
  return { offeringId: offeringId, date: tomorrow.getUTCFullYear() + '-' + mm + '-' + dd };
}

// 1. GET public availability (aggregate day variant). Shape-only asserts.
export function availability(data) {
  var url;
  if (data.offeringId) {
    url = BASE_URL + '/api/bookings/public/sites/' + SITE_SLUG +
      '/availability?offering_id=' + data.offeringId + '&date=' + data.date;
  } else {
    url = BASE_URL + '/api/bookings/public/sites/' + SITE_SLUG + '/';
  }
  var res = http.get(url, {
    tags: { leg: 'availability' },
    responseCallback: http.expectedStatuses(200, 404),
  });
  check(res, {
    'availability: 200/404': function (r) { return r.status === 200 || r.status === 404; },
    'availability: JSON body': function (r) {
      if (r.status !== 200) { return true; }
      try { JSON.parse(r.body); return true; } catch (e) { return false; }
    },
  });
}

// 2. POST waitlist claim (token capability surface). A random/nil token is
// rejected (400/404/410) — the assert is the rejection CONTRACT + latency,
// plus that the gateway rate limiter answers 429 rather than 5xx when hit.
export function waitlistClaim() {
  var payload = JSON.stringify({ token: '00000000-0000-0000-0000-000000000000' });
  var res = http.post(BASE_URL + '/api/bookings/v1/waitlist/claim', payload, {
    headers: jsonHeaders(),
    tags: { leg: 'waitlist_claim' },
    responseCallback: http.expectedStatuses(200, 400, 404, 410, 429),
  });
  check(res, {
    'claim: contract status': function (r) {
      return [200, 400, 404, 410, 429].indexOf(r.status) !== -1;
    },
    'claim: JSON body': function (r) {
      if (r.status === 429) { return true; }
      try { JSON.parse(r.body); return true; } catch (e) { return false; }
    },
  });
}

// 3. POST pay leg (W48-A A3). REAL endpoint, shape-only: 200 must carry
// {link, tx_ref}; 409 = already paid; 503 = Flutterwave rail unconfigured
// (valid dev answer, fail-closed); 404 = unknown booking (default env).
export function payLeg() {
  var payload = JSON.stringify({ email: 'loadtest@example.com' });
  var res = http.post(
    BASE_URL + '/api/bookings/public/sites/' + SITE_SLUG + '/bookings/' + BOOKING_ID + '/pay',
    payload,
    {
      headers: jsonHeaders(),
      tags: { leg: 'pay' },
      responseCallback: http.expectedStatuses(200, 400, 404, 409, 429, 503),
    }
  );
  check(res, {
    'pay: contract status': function (r) {
      return [200, 400, 404, 409, 429, 503].indexOf(r.status) !== -1;
    },
    'pay: 200 shape {link,tx_ref}': function (r) {
      if (r.status !== 200) { return true; }
      try {
        var b = JSON.parse(r.body);
        return typeof (b.link || (b.data && b.data.link)) === 'string' &&
          typeof (b.tx_ref || (b.data && b.data.tx_ref)) === 'string';
      } catch (e) { return false; }
    },
  });
}

// 4. GET public reviews aggregate (W48-A A4): {reviews, average, count},
// limit capped server-side at 50 — request 1000 to assert the cap holds.
export function reviews() {
  var res = http.get(
    BASE_URL + '/api/bookings/public/sites/' + SITE_SLUG + '/reviews?limit=1000&offset=0',
    {
      tags: { leg: 'reviews' },
      responseCallback: http.expectedStatuses(200, 404, 429),
    }
  );
  check(res, {
    'reviews: contract status': function (r) {
      return [200, 404, 429].indexOf(r.status) !== -1;
    },
    'reviews: aggregate shape + limit cap': function (r) {
      if (r.status !== 200) { return true; }
      try {
        var b = JSON.parse(r.body);
        var list = b.reviews || (b.data && b.data.reviews) || [];
        return Array.isArray(list) && list.length <= 50;
      } catch (e) { return false; }
    },
  });
}
