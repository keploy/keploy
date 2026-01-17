/**
 * Simple Express server that acts as an external API dependency.
 * This server is used to test Keploy's stub record/replay functionality with Playwright.
 */
const express = require('express');
const path = require('path');

const app = express();
app.use(express.json());

// Serve static files for UI tests
app.use(express.static(path.join(__dirname, 'public')));

const PORT = process.env.MOCK_SERVER_PORT || 9000;

// Sample data
const users = new Map([
  [1, { id: 1, name: 'Alice', email: 'alice@example.com', role: 'admin' }],
  [2, { id: 2, name: 'Bob', email: 'bob@example.com', role: 'user' }],
  [3, { id: 3, name: 'Charlie', email: 'charlie@example.com', role: 'user' }],
]);

const products = new Map([
  [1, { id: 1, name: 'Laptop', price: 999.99, stock: 50 }],
  [2, { id: 2, name: 'Phone', price: 699.99, stock: 100 }],
  [3, { id: 3, name: 'Tablet', price: 449.99, stock: 75 }],
]);

// Health check
app.get('/health', (req, res) => {
  res.json({ success: true, message: 'OK', timestamp: new Date().toISOString() });
});

// Users endpoints
app.get('/api/users', (req, res) => {
  const userList = Array.from(users.values());
  res.json({ success: true, data: userList, count: userList.length });
});

app.get('/api/users/:id', (req, res) => {
  const id = parseInt(req.params.id);
  const user = users.get(id);
  
  if (!user) {
    return res.status(404).json({ success: false, message: 'User not found' });
  }
  
  res.json({ success: true, data: user });
});

app.post('/api/users', (req, res) => {
  const { name, email, role } = req.body;
  
  if (!name || !email) {
    return res.status(400).json({ success: false, message: 'Name and email are required' });
  }
  
  const id = users.size + 1;
  const newUser = { id, name, email, role: role || 'user' };
  users.set(id, newUser);
  
  res.status(201).json({ success: true, data: newUser });
});

// Products endpoints
app.get('/api/products', (req, res) => {
  const productList = Array.from(products.values());
  res.json({ success: true, data: productList, count: productList.length });
});

app.get('/api/products/:id', (req, res) => {
  const id = parseInt(req.params.id);
  const product = products.get(id);
  
  if (!product) {
    return res.status(404).json({ success: false, message: 'Product not found' });
  }
  
  res.json({ success: true, data: product });
});

// Echo endpoint for testing request/response
app.all('/api/echo', (req, res) => {
  res.json({
    success: true,
    data: {
      method: req.method,
      path: req.path,
      query: req.query,
      headers: req.headers,
      body: req.body,
    }
  });
});

// Time endpoint (useful for testing time-sensitive mocks)
app.get('/api/time', (req, res) => {
  res.json({
    success: true,
    data: {
      timestamp: Date.now(),
      iso: new Date().toISOString(),
      unix: Math.floor(Date.now() / 1000),
    }
  });
});

// Search endpoint with query params
app.get('/api/search', (req, res) => {
  const { q, type } = req.query;
  
  let results = [];
  
  if (!type || type === 'users') {
    const userResults = Array.from(users.values())
      .filter(u => !q || u.name.toLowerCase().includes(q.toLowerCase()));
    results.push(...userResults.map(u => ({ ...u, type: 'user' })));
  }
  
  if (!type || type === 'products') {
    const productResults = Array.from(products.values())
      .filter(p => !q || p.name.toLowerCase().includes(q.toLowerCase()));
    results.push(...productResults.map(p => ({ ...p, type: 'product' })));
  }
  
  res.json({ success: true, data: results, query: q, count: results.length });
});

app.listen(PORT, () => {
  console.log(`Mock server running on port ${PORT}`);
});
