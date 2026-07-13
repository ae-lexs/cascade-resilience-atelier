import http from 'k6/http';
import { BASE } from './config.js';

// RETRIES=0 for baseline/single-layer; RETRIES=2 (→3 attempts) for the
// multi-layer cascade (Module 07). This client-side retry is Layer 3 of the 27×,
// deliberately part of the harness, not the app.
const RETRIES = Number(__ENV.RETRIES ?? 0);

// No steady-state failure threshold: this scenario runs during fault injection,
// where failures (and the retries they trigger) are exactly what we measure.
export const options = {
  scenarios: {
    cascade: { executor: 'constant-vus', vus: 20, duration: '10m' },
  },
};

export default function () {
  for (let i = 0; i <= RETRIES; i++) {
    const res = http.get(`${BASE}/echo`);
    if (res.status === 200) break; // stop on success; retry only on failure
  }
}
