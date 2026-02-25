#!/bin/bash
set -e

# Create k6 load test script
echo "📝 Creating k6 load test script..."

cat > load-test.js << 'EOF'
import http from 'k6/http';
import { check, sleep } from 'k6';

export const options = {
  scenarios: {
    constant_load: {
      executor: 'constant-arrival-rate',
      rate: 100,
      timeUnit: '1s',
      duration: '60s',
      preAllocatedVUs: 20,
      maxVUs: 100,
    },
  },
  // Track specific percentiles
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(80)', 'p(95)', 'p(99)', 'p(99.9)'],
  // Set thresholds for percentiles
  thresholds: {
    'http_req_duration': ['p(80)<100', 'p(99)<500'],
    'http_req_failed': ['rate<0.01'],
  },
};

// Array of PetClinic endpoints to hit
const endpoints = [
  '/',
  '/actuator/health',
  '/owners',
  '/vets',
  '/api/owners',
  '/api/vets',
];

export default function () {
  // Randomly select an endpoint to create diverse test cases
  const endpoint = endpoints[Math.floor(Math.random() * endpoints.length)];
  const res = http.get(`http://localhost:8080${endpoint}`);
  
  check(res, {
    'status is 2xx or 3xx': (r) => r.status >= 200 && r.status < 400,
  });
}
EOF

echo "✅ k6 load test script created: load-test.js"
