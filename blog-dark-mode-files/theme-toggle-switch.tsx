'use client'

import * as React from 'react'
import { useTheme } from 'next-themes'
import { Moon, Sun } from 'lucide-react'

export function ThemeToggleSwitch() {
  const { theme, setTheme, mounted } = useTheme()

  if (!mounted) {
    return <div className="w-14 h-7 bg-gray-200 dark:bg-gray-700 rounded-full" />
  }

  return (
    <button
      onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}
      className={`relative inline-flex h-7 w-14 items-center rounded-full transition-colors duration-300 ${
        theme === 'dark' 
          ? 'bg-slate-700 hover:bg-slate-600' 
          : 'bg-gray-300 hover:bg-gray-400'
      }`}
      aria-label={`Switch to ${theme === 'dark' ? 'light' : 'dark'} mode`}
      role="switch"
      aria-checked={theme === 'dark'}
    >
      <span
        className={`inline-flex h-6 w-6 transform items-center justify-center rounded-full bg-white shadow-md transition-transform duration-300 ${
          theme === 'dark' ? 'translate-x-7' : 'translate-x-0.5'
        }`}
      >
        {theme === 'dark' ? (
          <Moon className="h-3 w-3 text-blue-500" />
        ) : (
          <Sun className="h-3 w-3 text-amber-600" />
        )}
      </span>
    </button>
  )
}
