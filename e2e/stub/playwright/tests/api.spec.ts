import { test, expect, APIRequestContext } from '@playwright/test';

/**
 * These tests make HTTP API calls that will be intercepted by Keploy's proxy.
 * 
 * Usage with Keploy stub:
 * 
 * Record mocks:
 *   keploy stub record -c "npx playwright test" -p ./stubs --name api-tests
 * 
 * Replay mocks:
 *   keploy stub replay -c "npx playwright test" -p ./stubs --name api-tests
 */

test.describe('API Tests for Keploy Stub', () => {
  let request: APIRequestContext;
  const baseURL = process.env.API_BASE_URL || 'http://localhost:9000';

  test.beforeAll(async ({ playwright }) => {
    request = await playwright.request.newContext({
      baseURL: baseURL,
    });
  });

  test.afterAll(async () => {
    await request.dispose();
  });

  test.describe('Health Check', () => {
    test('should return healthy status', async () => {
      const response = await request.get('/health');
      expect(response.ok()).toBeTruthy();
      
      const body = await response.json();
      expect(body.success).toBe(true);
      expect(body.message).toBe('OK');
    });
  });

  test.describe('Users API', () => {
    test('should get all users', async () => {
      const response = await request.get('/api/users');
      expect(response.ok()).toBeTruthy();
      
      const body = await response.json();
      expect(body.success).toBe(true);
      expect(body.data).toBeInstanceOf(Array);
      expect(body.data.length).toBeGreaterThan(0);
      expect(body.count).toBe(body.data.length);
    });

    test('should get user by ID', async () => {
      const response = await request.get('/api/users/1');
      expect(response.ok()).toBeTruthy();
      
      const body = await response.json();
      expect(body.success).toBe(true);
      expect(body.data.id).toBe(1);
      expect(body.data.name).toBe('Alice');
      expect(body.data.email).toBe('alice@example.com');
    });

    test('should return 404 for non-existent user', async () => {
      const response = await request.get('/api/users/999');
      expect(response.status()).toBe(404);
      
      const body = await response.json();
      expect(body.success).toBe(false);
      expect(body.message).toBe('User not found');
    });

    test('should create a new user', async () => {
      const newUser = {
        name: 'David',
        email: 'david@example.com',
        role: 'user'
      };
      
      const response = await request.post('/api/users', {
        data: newUser
      });
      expect(response.status()).toBe(201);
      
      const body = await response.json();
      expect(body.success).toBe(true);
      expect(body.data.name).toBe(newUser.name);
      expect(body.data.email).toBe(newUser.email);
      expect(body.data.id).toBeDefined();
    });
  });

  test.describe('Products API', () => {
    test('should get all products', async () => {
      const response = await request.get('/api/products');
      expect(response.ok()).toBeTruthy();
      
      const body = await response.json();
      expect(body.success).toBe(true);
      expect(body.data).toBeInstanceOf(Array);
      expect(body.data.length).toBeGreaterThan(0);
    });

    test('should get product by ID', async () => {
      const response = await request.get('/api/products/1');
      expect(response.ok()).toBeTruthy();
      
      const body = await response.json();
      expect(body.success).toBe(true);
      expect(body.data.id).toBe(1);
      expect(body.data.name).toBe('Laptop');
      expect(body.data.price).toBe(999.99);
    });

    test('should return 404 for non-existent product', async () => {
      const response = await request.get('/api/products/999');
      expect(response.status()).toBe(404);
      
      const body = await response.json();
      expect(body.success).toBe(false);
    });
  });

  test.describe('Echo API', () => {
    test('should echo GET request', async () => {
      const response = await request.get('/api/echo?foo=bar&baz=qux');
      expect(response.ok()).toBeTruthy();
      
      const body = await response.json();
      expect(body.success).toBe(true);
      expect(body.data.method).toBe('GET');
      expect(body.data.query.foo).toBe('bar');
      expect(body.data.query.baz).toBe('qux');
    });

    test('should echo POST request with body', async () => {
      const testData = { message: 'Hello, Keploy!', number: 42 };
      
      const response = await request.post('/api/echo', {
        data: testData
      });
      expect(response.ok()).toBeTruthy();
      
      const body = await response.json();
      expect(body.success).toBe(true);
      expect(body.data.method).toBe('POST');
      expect(body.data.body.message).toBe(testData.message);
      expect(body.data.body.number).toBe(testData.number);
    });
  });

  test.describe('Search API', () => {
    test('should search without filters', async () => {
      const response = await request.get('/api/search');
      expect(response.ok()).toBeTruthy();
      
      const body = await response.json();
      expect(body.success).toBe(true);
      expect(body.data).toBeInstanceOf(Array);
    });

    test('should search with query parameter', async () => {
      const response = await request.get('/api/search?q=alice');
      expect(response.ok()).toBeTruthy();
      
      const body = await response.json();
      expect(body.success).toBe(true);
      expect(body.query).toBe('alice');
    });

    test('should search by type', async () => {
      const response = await request.get('/api/search?type=users');
      expect(response.ok()).toBeTruthy();
      
      const body = await response.json();
      expect(body.success).toBe(true);
      // All results should be users
      body.data.forEach((item: any) => {
        expect(item.type).toBe('user');
      });
    });

    test('should search products by name', async () => {
      const response = await request.get('/api/search?q=laptop&type=products');
      expect(response.ok()).toBeTruthy();
      
      const body = await response.json();
      expect(body.success).toBe(true);
      expect(body.data.length).toBeGreaterThan(0);
      expect(body.data[0].name).toBe('Laptop');
    });
  });

  test.describe('Time API', () => {
    test('should return current time', async () => {
      const response = await request.get('/api/time');
      expect(response.ok()).toBeTruthy();
      
      const body = await response.json();
      expect(body.success).toBe(true);
      expect(body.data.timestamp).toBeDefined();
      expect(body.data.iso).toBeDefined();
      expect(body.data.unix).toBeDefined();
    });
  });
});

test.describe('Sequential API Workflow', () => {
  let request: APIRequestContext;
  const baseURL = process.env.API_BASE_URL || 'http://localhost:9000';

  test.beforeAll(async ({ playwright }) => {
    request = await playwright.request.newContext({
      baseURL: baseURL,
    });
  });

  test.afterAll(async () => {
    await request.dispose();
  });

  test('should perform complete user workflow', async () => {
    // Step 1: Check health
    const healthResponse = await request.get('/health');
    expect(healthResponse.ok()).toBeTruthy();

    // Step 2: Get initial user count
    const usersResponse = await request.get('/api/users');
    const initialUsers = await usersResponse.json();
    const initialCount = initialUsers.data.length;

    // Step 3: Create a new user
    const createResponse = await request.post('/api/users', {
      data: { name: 'Workflow User', email: 'workflow@example.com' }
    });
    expect(createResponse.status()).toBe(201);
    const newUser = await createResponse.json();

    // Step 4: Verify user was created
    const verifyResponse = await request.get(`/api/users/${newUser.data.id}`);
    expect(verifyResponse.ok()).toBeTruthy();
    const verifiedUser = await verifyResponse.json();
    expect(verifiedUser.data.name).toBe('Workflow User');

    // Step 5: Search for the new user
    const searchResponse = await request.get('/api/search?q=workflow&type=users');
    expect(searchResponse.ok()).toBeTruthy();
  });

  test('should perform product search workflow', async () => {
    // Step 1: Get all products
    const productsResponse = await request.get('/api/products');
    expect(productsResponse.ok()).toBeTruthy();
    const products = await productsResponse.json();

    // Step 2: Get details of first product
    const firstProductId = products.data[0].id;
    const productResponse = await request.get(`/api/products/${firstProductId}`);
    expect(productResponse.ok()).toBeTruthy();

    // Step 3: Search for products
    const searchResponse = await request.get('/api/search?type=products');
    expect(searchResponse.ok()).toBeTruthy();
  });
});
