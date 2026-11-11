// loadtest/k6/smoke.js — k6 health sweep of every service in the dev stack
// (SPEC-W48-D, recon F8). Hits each service's /healthz directly on the
// compose network (backends are intentionally NOT host-published, GF4 /
// SPEC-W34), so run k6 in-network — see loadtest/README.md.
//
// Core services must be healthy (threshold-gated); overlay/optional services
// (graph-service runs only under the graph compose file, credit-bureau under
// the experimental profile) are probed and reported but never fail the run.
//
// Config (env): each URL is overridable via <NAME>_URL (e.g. BOOKING_URL).
//   DURATION (default 30s), VUS (default 1).
//
// k6 JS (goja) — plain ES5.1-compatible JavaScript, no TypeScript syntax.

import http from 'k6/http';
import { check } from 'k6';

function svc(name, url, tier, okStatuses) {
  return {
    name: name,
    url: __ENV[name.toUpperCase().replace(/-/g, '_') + '_URL'] || url,
    tier: tier,
    ok: okStatuses,
  };
}

var SERVICES = [
  svc('identity',          'http://identity:7001/healthz',          'core', [200]),
  svc('booking',           'http://booking:7002/healthz',           'core', [200]),
  svc('notification',      'http://notification:7003/healthz',      'core', [200]),
  svc('payments',          'http://payments:7004/healthz',          'core', [200]),
  svc('edge',              'http://edge:7005/healthz',              'core', [200]),
  svc('voice',             'http://voice:7006/healthz',             'core', [200]),
  svc('conversation',      'http://conversation:7007/healthz',      'core', [200]),
  svc('knowledge',         'http://knowledge:7008/healthz',         'core', [200]),
  svc('analytics',         'http://analytics:7009/healthz',         'core', [200]),
  svc('crm-sync',          'http://crm-sync:7010/healthz',          'core', [200]),
  svc('messaging-gateway', 'http://messaging-gateway:7011/healthz', 'core', [200]),
  svc('billing-engine',    'http://billing-engine:7012/healthz',    'core', [200]),
  svc('kyc',               'http://kyc:7013/healthz',               'core', [200]),
  svc('web',               'http://web:3001/',                      'core', [200, 301, 302, 307, 308]),
  // Optional overlays: absent from the base stack by design.
  svc('graph-service',     'http://graph-service:7014/healthz',     'optional', [200]),
  svc('credit-bureau',     'http://credit-bureau:7022/healthz',     'optional', [200]),
];

export var options = {
  scenarios: {
    sweep: {
      executor: 'constant-vus',
      vus: parseInt(__ENV.VUS || '1', 10),
      duration: __ENV.DURATION || '30s',
      exec: 'sweep',
    },
  },
  thresholds: {
    // Health endpoints must stay far inside the SLO-1 envelope
    // (docs/performance-budgets.md: p99 <= 1.5 s).
    http_req_duration: ['p(99)<1500'],
    // Core tier: tolerate essentially zero failures.
    'http_req_failed{tier:core}': ['rate<0.01'],
    checks: ['rate>0.99'],
  },
};

export function sweep() {
  for (var i = 0; i < SERVICES.length; i++) {
    var s = SERVICES[i];
    var res = http.get(s.url, {
      tags: { svc: s.name, tier: s.tier },
      // Expected-statuses only gate the metric accounting for HTTP-level
      // answers; connection failures on optional services are handled by
      // the tier check below (connection errors yield status 0).
      responseCallback: http.expectedStatuses.apply(null, s.ok),
    });
    var healthy = s.ok.indexOf(res.status) !== -1;
    if (s.tier === 'core') {
      check(res, { 'core service healthy': function () { return healthy; } }, { svc: s.name });
    } else {
      check(res, { 'optional service probed': function () { return true; } }, { svc: s.name });
      if (!healthy) {
        // Informational only — optional services may be absent by design.
        console.log('optional service not healthy (ok if overlay not running): ' + s.name + ' -> ' + res.status);
      }
    }
  }
}
