// Centralized k6 config: the target and the steady-state thresholds every
// non-fault scenario shares. The scenarios import from here so the ALB URL and
// pass/fail bar are defined in exactly one place.
export const BASE = __ENV.ALB_URL;

// Steady-state bar: <1% failed requests. Applied by scenarios that run WITHOUT
// fault injection (sustained). The cascade scenario deliberately omits this —
// it runs during induced faults where failures are the whole point.
export const steadyStateThresholds = { http_req_failed: ['rate<0.01'] };
