import { wrappedNodeFetch } from '@octokit/node-fetch';

describe('wrappedNodeFetch', () => {
  it('should make a successful HTTP request', async () => {
    const url = 'https://api.github.com/users/octocat';
    const response = await wrappedNodeFetch(url);
    expect(response.status).toBe(200);
    expect(response.headers.get('content-type')).toContain('application/json');
    const body = await response.json();
    expect(body.login).toBe('octocat');
  });

  it('should handle HTTP errors', async () => {
    const url = 'https://api.github.com/users/nonexistentuser';
    try {
      await wrappedNodeFetch(url);
    } catch (error) {
      expect(error.message).toContain('404');
      expect(error.status).toBe(404);
      expect(error.headers.get('content-type')).toContain('application/json');
      const body = await error.json();
      expect(body.message).toBe('Not Found');
    }
  });
});




