// Service Worker for offline caching
const CACHE_NAME = 'klt-dashboard-v4';
const STATIC_CACHE_URLS = [
  '/',
  '/index.html',
  '/favicon.ico',
  '/manifest.json'
];

// Install event - cache static resources
self.addEventListener('install', (event) => {
  console.log('Service Worker installing...');
  event.waitUntil(
    caches.open(CACHE_NAME)
      .then((cache) => {
        console.log('Caching static resources');
        // Try to cache the basic URLs, but don't fail if some are missing
        return Promise.all(
          STATIC_CACHE_URLS.map(url => {
            return cache.add(url).catch(err => {
              console.warn('Failed to cache:', url, err);
            });
          })
        );
      })
      .then(() => {
        console.log('Service Worker installation complete');
        // Force the waiting service worker to become the active service worker
        return self.skipWaiting();
      })
      .catch(err => {
        console.error('Service Worker installation failed:', err);
      })
  );
});

// Activate event - clean up old caches
self.addEventListener('activate', (event) => {
  console.log('Service Worker activating...');
  event.waitUntil(
    caches.keys()
      .then((cacheNames) => {
        return Promise.all(
          cacheNames.map((cacheName) => {
            if (cacheName !== CACHE_NAME) {
              console.log('Deleting old cache:', cacheName);
              return caches.delete(cacheName);
            }
          })
        );
      })
      .then(() => {
        console.log('Service Worker activation complete');
        // Ensure the new service worker takes control immediately
        return self.clients.claim();
      })
  );
});

// Fetch event - Selective caching to avoid interfering with app functionality
self.addEventListener('fetch', (event) => {
  // Skip cross-origin requests
  if (!event.request.url.startsWith(self.location.origin)) {
    return;
  }

  // Skip chrome-extension and other non-http requests
  if (!event.request.url.startsWith('http')) {
    return;
  }

  // Skip requests that shouldn't be cached (to avoid interfering with app logic)
  const url = new URL(event.request.url);
  
  // Skip API calls, WebSocket connections, and other dynamic requests
  if (
    url.pathname.startsWith('/metrics/') ||
    event.request.headers.get('cache-control') === 'no-cache' ||
    event.request.headers.get('cache-control') === 'no-store'
  ) {
    console.log('🚫 Skipping service worker for:', event.request.url);
    return; // Let the browser handle these normally
  }

  // Only cache static assets and navigation requests
  if (shouldCacheRequest(event.request)) {
    event.respondWith(networkFirstStrategy(event.request));
  }
});

// Determine if a request should be cached
function shouldCacheRequest(request) {
  const url = new URL(request.url);
  
  // Cache navigation requests (HTML pages)
  if (request.destination === 'document') {
    return true;
  }
  
  // Cache static assets
  if (
    request.destination === 'script' ||
    request.destination === 'style' ||
    request.destination === 'image' ||
    request.destination === 'font' ||
    url.pathname.includes('/_next/static/') ||
    url.pathname.endsWith('.css') ||
    url.pathname.endsWith('.js') ||
    url.pathname.endsWith('.ico') ||
    url.pathname.endsWith('.png') ||
    url.pathname.endsWith('.jpg') ||
    url.pathname.endsWith('.svg') ||
    url.pathname.endsWith('.woff') ||
    url.pathname.endsWith('.woff2')
  ) {
    return true;
  }
  
  // Cache manifest and service worker files
  if (
    url.pathname === '/manifest.json' ||
    url.pathname === '/sw.js'
  ) {
    return true;
  }
  
  return false;
}

// Network first strategy - Always try network first, fallback to cache only when server is down
async function networkFirstStrategy(request) {
  try {
    const response = await fetch(request);
    
    if (response && response.status === 200) {
      // Only cache successful responses for static content
      if (shouldCacheResponse(request, response)) {
        const cache = await caches.open(CACHE_NAME);
        cache.put(request, response.clone());
        console.log('📦 Cached fresh content:', request.url);
      }
      
      return response;
    } else {
      // Bad response, try cache
      const cachedResponse = await caches.match(request);
      return cachedResponse || response;
    }
  } catch (error) {
    // Network failed (server down), use cache
    const cachedResponse = await caches.match(request);
    
    if (cachedResponse) {
      console.log('📱 Serving offline content:', request.url);
      return cachedResponse;
    }
    
    // If it's a navigation request and no cache, try to serve the main page
    if (request.destination === 'document') {
      const mainPage = await caches.match('/');
      if (mainPage) {
        console.log('🏠 Serving main page as fallback');
        return mainPage;
      }
    }
    
    throw error;
  }
}

// Determine if a response should be cached
function shouldCacheResponse(request, response) {
  // Don't cache if response has cache-control: no-store
  const cacheControl = response.headers.get('cache-control');
  if (cacheControl && cacheControl.includes('no-store')) {
    return false;
  }
  
  // Cache static assets and navigation requests
  return shouldCacheRequest(request);
}

// Listen for messages from the main thread
self.addEventListener('message', (event) => {
  if (event.data && event.data.type === 'SKIP_WAITING') {
    self.skipWaiting();
  }
});
