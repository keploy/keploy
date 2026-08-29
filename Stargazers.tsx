'use client';

import { useEffect, useState } from 'react';
import { getStargazers } from '../services/githubServices';

export function Stargazers() {
  const [stargazers, setStargazers] = useState([]);

  useEffect(() => {
    getStargazers().then(setStargazers);
  }, []);

  return (
    <div className="grid grid-cols-2 gap-4">
      {stargazers.map((user: any) => (
        <div key={user.id} className="border p-2 rounded">
          <img src={user.avatar_url} alt={user.login} className="w-12 h-12 rounded-full" />
          <p>{user.login}</p>
        </div>
      ))}
    </div>
  );
}
