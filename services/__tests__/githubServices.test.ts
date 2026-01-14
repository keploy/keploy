import { getStargazers } from '../githubServices';

global.fetch = jest.fn(() =>
  Promise.resolve({
    ok: true,
    json: () => Promise.resolve([{ id: 1, login: 'test-user', avatar_url: 'https://example.com/avatar.png' }]),
  })
) as jest.Mock;

describe('getStargazers', () => {
  it('fetches stargazers successfully', async () => {
    const data = await getStargazers();
    expect(data).toBeDefined();
    expect(data.length).toBeGreaterThan(0);
    expect(data[0].login).toBe('test-user');
  });

  it('throws an error when fetch fails', async () => {
    (global.fetch as jest.Mock).mockImplementationOnce(() =>
      Promise.resolve({
        ok: false,
      })
    );

    await expect(getStargazers()).rejects.toThrow('Failed to fetch');
  });
});
