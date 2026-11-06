import http from 'k6';
import { check, sleep } from 'k6';

const baseURL = (__ENV.BASE_URL || '').replace(/\/+$/, '');
const apiKey = __ENV.GATEWAY_API_KEY || '';
const model = __ENV.MODEL || '';
const maxVUs = Number.parseInt(__ENV.MAX_VUS || '25', 10);
const rampSeconds = Number.parseInt(__ENV.RAMP_SECONDS || '30', 10);
const holdSeconds = Number.parseInt(__ENV.HOLD_SECONDS || '120', 10);
const sleepSeconds = Number.parseFloat(__ENV.SLEEP_SECONDS || '0.2');
const timeout = __ENV.REQUEST_TIMEOUT || '120s';
const p95Limit = Number.parseInt(__ENV.P95_LIMIT_MS || '30000', 10);

if (!baseURL || !apiKey || !model) {
  throw new Error('Set BASE_URL, GATEWAY_API_KEY and MODEL before running this load test.');
}
if (![maxVUs, rampSeconds, holdSeconds, p95Limit].every(Number.isFinite) ||
    maxVUs < 1 || rampSeconds < 1 || holdSeconds < 1 || p95Limit < 1 ||
    !Number.isFinite(sleepSeconds) || sleepSeconds < 0) {
  throw new Error('Load-test numeric settings must be positive (SLEEP_SECONDS may be zero).');
}

export const options = {
  discardResponseBodies: true,
  scenarios: {
    gateway_load: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: `${rampSeconds}s`, target: maxVUs },
        { duration: `${holdSeconds}s`, target: maxVUs },
        { duration: `${rampSeconds}s`, target: 0 },
      ],
      gracefulRampDown: '30s',
    },
  },
  thresholds: {
    checks: ['rate>0.99'],
    http_req_failed: ['rate<0.01'],
    http_req_duration: [`p(95)<${p95Limit}`],
  },
};

const payload = JSON.stringify({
  model,
  messages: [{ role: 'user', content: 'Reply with exactly: OK' }],
  max_tokens: 16,
  temperature: 0,
  stream: false,
});

export default function () {
  const response = http.post(`${baseURL}/v1/chat/completions`, payload, {
    headers: {
      Authorization: `Bearer ${apiKey}`,
      'Content-Type': 'application/json',
      Accept: 'application/json',
    },
    timeout,
    tags: { endpoint: 'chat_completions', model },
  });

  check(response, {
    'gateway returned HTTP 200': (r) => r.status === 200,
  });

  if (sleepSeconds > 0) sleep(sleepSeconds);
}
