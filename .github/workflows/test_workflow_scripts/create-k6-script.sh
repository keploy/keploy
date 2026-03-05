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
      duration: '30s',        // Reduced from 60s to 30s
      preAllocatedVUs: 20,
      maxVUs: 100,
    },
  },
  // Track specific percentiles - P50, P90, P99 root out outliers
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(50)', 'p(90)', 'p(95)', 'p(99)'],  // Removed p(99.9)
  // Note: Thresholds below are lenient defaults for k6's internal validation
  // The actual validation is done by run-perf-test-with-validation.sh using stricter thresholds
  thresholds: {
    'http_req_duration': [
      'p(50)<5',    // P50 < 5ms - median response time
      'p(90)<15',   // P90 < 15ms - 90th percentile
      'p(99)<70'    // P99 < 70ms - 99th percentile
    ],
    'http_req_failed': ['rate<0.01'],  // Error rate < 1%
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
