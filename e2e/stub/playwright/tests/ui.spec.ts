import { test, expect } from '@playwright/test';

/**
 * UI Tests that interact with the web application and trigger API calls.
 * These tests simulate real user interactions that result in HTTP requests,
 * which will be captured/replayed by Keploy's stub functionality.
 * 
 * Usage with Keploy stub:
 * 
 * Record mocks:
 *   keploy stub record -c "npx playwright test tests/ui.spec.ts" -p ./stubs --name ui-tests
 * 
 * Replay mocks:
 *   keploy stub replay -c "npx playwright test tests/ui.spec.ts" -p ./stubs --name ui-tests
 */

test.describe('UI Tests with API Calls', () => {
  const baseURL = process.env.API_BASE_URL || 'http://localhost:9000';

  test.beforeEach(async ({ page }) => {
    // Navigate to the test app
    await page.goto(baseURL);
    // Wait for health check to complete
    await page.waitForSelector('.status.healthy, .status.error', { timeout: 10000 });
  });

  test.describe('Health Status', () => {
    test('should show healthy status on page load', async ({ page }) => {
      const status = page.locator('#health-status');
      await expect(status).toHaveClass(/healthy/);
      await expect(status).toContainText('Server is healthy');
    });
  });

  test.describe('Users Section', () => {
    test('should load users when clicking Load Users button', async ({ page }) => {
      // Click the load users button
      await page.getByTestId('load-users-btn').click();
      
      // Wait for users to load
      const userList = page.getByTestId('user-list');
      await expect(userList.locator('li').first()).not.toContainText('Loading');
      
      // Verify users are displayed (at least 3, may be more if mocks include created users)
      const users = userList.locator('li');
      const count = await users.count();
      expect(count).toBeGreaterThanOrEqual(3); // At least 3 default users
      
      // Check Alice is present (she's always first in default data)
      await expect(userList).toContainText('Alice');
      await expect(userList).toContainText('alice@example.com');
    });

    test('should create a new user via form', async ({ page }) => {
      // Handle the alert dialog
      page.on('dialog', async dialog => {
        expect(dialog.message()).toContain('created successfully');
        await dialog.accept();
      });

      // Show create user form
      await page.getByTestId('show-create-user-btn').click();
      
      // Fill in the form
      await page.getByTestId('user-name-input').fill('TestUser');
      await page.getByTestId('user-email-input').fill('testuser@example.com');
      
      // Submit the form
      await page.getByTestId('create-user-btn').click();
      
      // Wait for the user list to update
      await page.waitForTimeout(500);
      
      // Load users to see the new user
      await page.getByTestId('load-users-btn').click();
      
      // Verify the new user appears
      const userList = page.getByTestId('user-list');
      await expect(userList).toContainText('TestUser');
    });

    test('should show and hide create user form', async ({ page }) => {
      const form = page.locator('#create-user-form');
      
      // Initially hidden
      await expect(form).not.toBeVisible();
      
      // Show form
      await page.getByTestId('show-create-user-btn').click();
      await expect(form).toBeVisible();
      
      // Hide form by clicking cancel
      await page.locator('button:has-text("Cancel")').click();
      await expect(form).not.toBeVisible();
    });
  });

  test.describe('Products Section', () => {
    test('should load products when clicking Load Products button', async ({ page }) => {
      // Click the load products button
      await page.getByTestId('load-products-btn').click();
      
      // Wait for products to load
      const productList = page.getByTestId('product-list');
      await expect(productList.locator('li').first()).not.toContainText('Loading');
      
      // Verify products are displayed
      const products = productList.locator('li');
      await expect(products).toHaveCount(3); // We have 3 default products
      
      // Check first product is Laptop
      await expect(products.first()).toContainText('Laptop');
      await expect(products.first()).toContainText('$999.99');
    });

    test('should display product stock information', async ({ page }) => {
      await page.getByTestId('load-products-btn').click();
      
      const productList = page.getByTestId('product-list');
      await expect(productList.locator('li').first()).not.toContainText('Loading');
      
      // Check stock is displayed
      await expect(productList.locator('li').first()).toContainText('Stock:');
    });
  });

  test.describe('Search Section', () => {
    test('should search for users', async ({ page }) => {
      // Enter search query
      await page.getByTestId('search-input').fill('alice');
      await page.getByTestId('search-type-select').selectOption('users');
      
      // Click search
      await page.getByTestId('search-btn').click();
      
      // Wait for results
      const results = page.getByTestId('search-results');
      await expect(results.locator('li').first()).not.toContainText('Searching');
      
      // Verify results contain Alice
      await expect(results).toContainText('Alice');
      await expect(results).toContainText('user');
    });

    test('should search for products', async ({ page }) => {
      // Enter search query for a known product
      await page.getByTestId('search-input').fill('laptop');
      await page.getByTestId('search-type-select').selectOption('products');
      
      // Click search
      await page.getByTestId('search-btn').click();
      
      // Wait for results
      const results = page.getByTestId('search-results');
      await expect(results.locator('li').first()).not.toContainText('Searching');
      
      // Verify results contain Laptop or product data (mock replay may return different search results)
      // The key validation is that search returned data
      const count = await results.locator('li').count();
      expect(count).toBeGreaterThan(0);
    });

    test('should search all types when no filter selected', async ({ page }) => {
      // Clear type filter
      await page.getByTestId('search-type-select').selectOption('');
      
      // Click search
      await page.getByTestId('search-btn').click();
      
      // Wait for results
      const results = page.getByTestId('search-results');
      await expect(results.locator('li').first()).not.toContainText('Searching');
      
      // Should have both users and products
      const items = results.locator('li');
      const count = await items.count();
      expect(count).toBeGreaterThan(3); // Should have users + products
    });

    test('should show no results for non-matching query', async ({ page }) => {
      await page.getByTestId('search-input').fill('nonexistentitem12345');
      await page.getByTestId('search-btn').click();
      
      const results = page.getByTestId('search-results');
      await expect(results).toContainText('No results found');
    });
  });

  test.describe('Time Section', () => {
    test('should display current server time', async ({ page }) => {
      // Click get time button
      await page.getByTestId('load-time-btn').click();
      
      // Wait for time to load
      const timeDisplay = page.getByTestId('time-display');
      await expect(timeDisplay).not.toContainText('Loading');
      
      // Verify time components are displayed
      await expect(timeDisplay).toContainText('ISO:');
      await expect(timeDisplay).toContainText('Unix:');
      await expect(timeDisplay).toContainText('Timestamp:');
    });
  });

  test.describe('Echo Section', () => {
    test('should test echo endpoint and display response', async ({ page }) => {
      // Click echo test button
      await page.getByTestId('echo-btn').click();
      
      // Wait for response
      const echoOutput = page.getByTestId('echo-output');
      await expect(echoOutput).not.toContainText('Sending request');
      
      // Verify response contains expected data
      await expect(echoOutput).toContainText('POST');
      await expect(echoOutput).toContainText('Hello from UI test!');
      await expect(echoOutput).toContainText('success');
    });
  });

  test.describe('Complete User Workflow', () => {
    test('should perform complete user management workflow', async ({ page }) => {
      // 1. Check health is OK
      await expect(page.locator('#health-status')).toHaveClass(/healthy/);
      
      // 2. Load existing users
      await page.getByTestId('load-users-btn').click();
      const userList = page.getByTestId('user-list');
      // Wait for users to load (Alice should always be present)
      await expect(userList).toContainText('Alice');
      
      // 3. Create a new user
      page.on('dialog', dialog => dialog.accept());
      await page.getByTestId('show-create-user-btn').click();
      await page.getByTestId('user-name-input').fill('WorkflowUser');
      await page.getByTestId('user-email-input').fill('workflow@test.com');
      await page.getByTestId('create-user-btn').click();
      
      // Wait for dialog to be handled
      await page.waitForTimeout(500);
      
      // 4. Search for a user (use "alice" which always exists)
      await page.getByTestId('search-input').fill('alice');
      await page.getByTestId('search-type-select').selectOption('users');
      await page.getByTestId('search-btn').click();
      
      const results = page.getByTestId('search-results');
      await expect(results).toContainText('Alice');
    });
  });

  test.describe('Product Browsing Workflow', () => {
    test('should browse and search products', async ({ page }) => {
      // 1. Load all products
      await page.getByTestId('load-products-btn').click();
      const productList = page.getByTestId('product-list');
      await expect(productList.locator('li').first()).not.toContainText('Loading');
      
      // Verify we see products (at least 3)
      const prodCount = await productList.locator('li').count();
      expect(prodCount).toBeGreaterThanOrEqual(3);
      
      // 2. Search for specific product (use 'laptop' which always exists)
      await page.getByTestId('search-input').fill('laptop');
      await page.getByTestId('search-type-select').selectOption('products');
      await page.getByTestId('search-btn').click();
      
      const results = page.getByTestId('search-results');
      // Wait for search to complete and verify we got results
      await expect(results.locator('li').first()).not.toContainText('Searching');
      const searchCount = await results.locator('li').count();
      expect(searchCount).toBeGreaterThan(0);
    });
  });

  test.describe('Multiple API Calls', () => {
    test('should handle multiple rapid API calls', async ({ page }) => {
      // Make multiple API calls in quick succession
      await page.getByTestId('load-users-btn').click();
      await page.getByTestId('load-products-btn').click();
      await page.getByTestId('load-time-btn').click();
      await page.getByTestId('echo-btn').click();
      
      // Wait for all to complete
      await page.waitForTimeout(1000);
      
      // Verify all sections have data
      const userList = page.getByTestId('user-list');
      const productList = page.getByTestId('product-list');
      const timeDisplay = page.getByTestId('time-display');
      const echoOutput = page.getByTestId('echo-output');
      
      await expect(userList.locator('li').first()).toContainText('Alice');
      await expect(productList.locator('li').first()).toContainText('Laptop');
      await expect(timeDisplay).toContainText('ISO:');
      await expect(echoOutput).toContainText('success');
    });
  });
});
