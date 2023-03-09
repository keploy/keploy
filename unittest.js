const { wrappedNodeFetch } = require('@octokit/request');

describe('wrappedNodeFetch', () => {
  it('returns a promise that resolves to the response object', async () => {
      


    const url = 'https://api.github.com/users/octocat';
    const options = {
      headers: {
        Accept: 'application/vnd.github.v3+json',
        Authorization: `token ${process.env.GITHUB_TOKEN}`,
      },
    };

    


    const response = await wrappedNodeFetch(url, options);

    


    expect(response.status).toBe(200);
    expect(response.statusText).toBe('OK');
    expect(response.headers.get('content-type')).toMatch(/application\/json/);

    const body = await response.json();
    expect(body).toHaveProperty('login', 'octocat');
  });

  it('throws an error for non-200 status codes', async () => {
    


    const url = 'https://api.github.com/users/nonexistentuser';
    const options = {
      headers: {
        Accept: 'application/vnd.github.v3+json',
        Authorization: `token ${process.env.GITHUB_TOKEN}`,
      },
    };

    

    
    await expect(wrappedNodeFetch(url, options)).rejects.toThrow();
  });
});