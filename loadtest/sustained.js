import http from 'k6/http';
import { check } from 'k6';
import { BASE, steadyStateThresholds } from './config.js';

export const options = {
  scenarios: {
    sustained: { executor: 'constant-vus', vus: 20, duration: '10m' },
  },
  thresholds: steadyStateThresholds,
};

export default function () {
  const res = http.get(`${BASE}/echo`);
  check(res, { 'status 200': (r) => r.status === 200 });
}
