import http from 'k6/http';
import { BASE } from './config.js';

// RETRIES=0 for baseline/single-layer; RETRIES=2 (→3 attempts) for the
// multi-layer cascade (Module 07). This client-side retry is Layer 3 of the 27×,
// deliberately part of the harness, not the app.
const RETRIES = Number(__ENV.RETRIES ?? 0);

// A per-run random prefix; combined with k6's per-iteration (__VU, __ITER)
// coordinates — unique within a run — it yields a collision-free origin id
// without a remote jslib import (one fewer network dependency in the harness).
const RUN = `${Date.now().toString(36)}${Math.floor(Math.random() * 1e6).toString(36)}`;

// No steady-state failure threshold: this scenario runs during fault injection,
// where failures (and the retries they trigger) are exactly what we measure.
export const options = {
  scenarios: {
    cascade: { executor: 'constant-vus', vus: 20, duration: '10m' },
  },
};

export default function () {
  // ONE origin id per logical request, reused across every Layer-3 retry, so the
  // amplification denominator counts ORIGINATING requests, not client attempts.
  // The edge forwards this X-Request-Id and chi's RequestID middleware reuses it
  // at every hop, so all ~27 db_attempt lines carry the same id (§VI.2).
  const originId = `${RUN}-${__VU}-${__ITER}`;
  // timeout must exceed one full Layer-2 cascade (~20s worst case, §V.4), or k6
  // abandons the edge mid-cascade and its "retry" starts a fresh one — truncating
  // the product toward the <15× falsifier.
  const params = { headers: { 'X-Request-Id': originId }, timeout: '35s' };
  for (let i = 0; i <= RETRIES; i++) {
    const res = http.get(`${BASE}/echo`, params);
    if (res.status === 200) break; // stop on success; retry only on failure
  }
}
