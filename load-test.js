import http from 'k6/http';
import { check } from 'k6';

export const options = {
  scenarios: {
    constant_load: {
      executor: 'constant-arrival-rate',
      rate: 100,
      timeUnit: '1s',
      duration: '60s',
      preAllocatedVUs: 10,
      maxVUs: 50,
    },
  },
};

export default function () {
  const res = http.get('http://localhost:8080/');
  check(res, {
    'status is 200': (r) => r.status === 200,
  });
}
