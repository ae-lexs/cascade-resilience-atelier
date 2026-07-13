import http from 'k6/http';
import { check } from 'k6';
import { BASE, steadyStateThresholds } from './config.js';

// Burst: a short traffic spike over the steady state, to surface queueing and
// saturation the constant-VU sustained run won't. Open model (arrival-rate) so
// the spike is real request pressure, not gated by response latency. Runs
// WITHOUT fault injection, so it keeps the steady-state <1% failure bar — the
// question is whether the spike alone stays healthy.
export const options = {
  scenarios: {
    burst: {
      executor: 'ramping-arrival-rate',
      startRate: 20,            // requests/sec — roughly the sustained baseline
      timeUnit: '1s',
      preAllocatedVUs: 50,
      maxVUs: 200,
      stages: [
        { target: 20, duration: '30s' },   // warm up at steady state
        { target: 200, duration: '15s' },  // ramp into the spike
        { target: 200, duration: '30s' },  // hold the burst
        { target: 20, duration: '15s' },   // recover
      ],
    },
  },
  thresholds: steadyStateThresholds,
};

export default function () {
  const res = http.get(`${BASE}/echo`);
  check(res, { 'status 200': (r) => r.status === 200 });
}
