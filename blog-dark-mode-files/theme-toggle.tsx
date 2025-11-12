'use client'

import * as React from 'react'
import { useTheme } from 'next-themes'
import { Button } from '@/components/ui/button'
import { Moon, Sun } from 'lucide-react'

export function ThemeToggle() {
  const { theme, setTheme, mounted } = useTheme()
  const [isOpen, setIsOpen] = React.useState(false)

  // Prevent hydration mismatch
  if (!mounted) {
    return <div className="w-9 h-9" />
  }

  const toggleTheme = (newTheme: string) => {
    setTheme(newTheme)
    setIsOpen(false)
  }

  return (
    <div className="relative inline-block">
      <Button
        variant="ghost"
        size="icon"
        onClick={() => setIsOpen(!isOpen)}
        className="relative w-9 h-9 rounded-full hover:bg-gray-100 dark:hover:bg-gray-800 transition-colors"
        aria-label="Toggle theme menu"
      >
        {theme === 'dark' ? (
          <Moon className="h-5 w-5 text-slate-700 dark:text-slate-300" />
        ) : (
          <Sun className="h-5 w-5 text-amber-600" />
        )}
      </Button>

      {/* Dropdown menu */}
      {isOpen && (
        <div className="absolute right-0 mt-2 w-48 bg-white dark:bg-slate-900 border border-gray-200 dark:border-slate-700 rounded-lg shadow-lg z-50">
          <button
            onClick={() => toggleTheme('light')}
            className={`w-full text-left px-4 py-2 hover:bg-gray-100 dark:hover:bg-slate-800 transition-colors flex items-center gap-2 rounded-t-lg ${
              theme === 'light' ? 'bg-gray-100 dark:bg-slate-800 font-semibold' : ''
            }`}
          >
            <Sun className="h-4 w-4 text-amber-600" />
            <span>Light</span>
          </button>
          <button
            onClick={() => toggleTheme('dark')}
            className={`w-full text-left px-4 py-2 hover:bg-gray-100 dark:hover:bg-slate-800 transition-colors flex items-center gap-2 ${
              theme === 'dark' ? 'bg-gray-100 dark:bg-slate-800 font-semibold' : ''
            }`}
          >
            <Moon className="h-4 w-4 text-blue-400" />
            <span>Dark</span>
          </button>
          <button
            onClick={() => toggleTheme('system')}
            className={`w-full text-left px-4 py-2 hover:bg-gray-100 dark:hover:bg-slate-800 transition-colors flex items-center gap-2 rounded-b-lg ${
              theme === 'system' ? 'bg-gray-100 dark:bg-slate-800 font-semibold' : ''
            }`}
          >
            <span className="text-lg">🖥️</span>
            <span>System</span>
          </button>
        </div>
      )}
    </div>
  )
}
