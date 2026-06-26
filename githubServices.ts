export async function getStargazers() {
  const res = await fetch('https://api.github.com/repos/keploy/keploy/stargazers');
  if (!res.ok) {
    throw new Error('Failed to fetch');
  }
  return res.json();
}
