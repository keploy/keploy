import { Stargazers } from '../components/Stargazers';

export default function Home() {
  return (
    <main className="p-4">
      <h1 className="text-2xl font-bold mb-4">Keploy Stargazers</h1>
      <Stargazers />
    </main>
  );
}
