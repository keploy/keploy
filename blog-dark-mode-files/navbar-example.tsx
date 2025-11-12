'use client'

import Link from 'next/link'
import { ThemeToggle } from '@/components/theme-toggle'
// Alternative: import { ThemeToggleSwitch } from '@/components/theme-toggle-switch'
import { useState } from 'react'
import { Menu, X } from 'lucide-react'

export function Navbar() {
  const [isMobileMenuOpen, setIsMobileMenuOpen] = useState(false)

  return (
    <nav className="border-b border-gray-200 dark:border-slate-800 sticky top-0 bg-white dark:bg-slate-950 z-50 shadow-sm dark:shadow-lg">
      <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
        <div className="flex justify-between items-center h-16">
          {/* Logo */}
          <div className="flex-shrink-0">
            <Link href="/" className="text-2xl font-bold bg-gradient-to-r from-keploy-primary to-blue-600 bg-clip-text text-transparent hover:opacity-80 transition-opacity">
              Keploy
            </Link>
          </div>

          {/* Desktop Navigation */}
          <div className="hidden md:flex items-center gap-8">
            <Link
              href="/blog"
              className="text-gray-700 dark:text-gray-300 hover:text-keploy-primary dark:hover:text-blue-400 transition-colors"
            >
              Blog
            </Link>
            <Link
              href="/docs"
              className="text-gray-700 dark:text-gray-300 hover:text-keploy-primary dark:hover:text-blue-400 transition-colors"
            >
              Documentation
            </Link>
            <Link
              href="/community"
              className="text-gray-700 dark:text-gray-300 hover:text-keploy-primary dark:hover:text-blue-400 transition-colors"
            >
              Community
            </Link>
          </div>

          {/* Right side - Theme Toggle and Mobile Menu */}
          <div className="flex items-center gap-4">
            {/* Theme Toggle */}
            <ThemeToggle />

            {/* Mobile Menu Button */}
            <button
              className="md:hidden inline-flex items-center justify-center p-2 rounded-md hover:bg-gray-100 dark:hover:bg-slate-800 transition-colors"
              onClick={() => setIsMobileMenuOpen(!isMobileMenuOpen)}
              aria-label="Toggle menu"
            >
              {isMobileMenuOpen ? (
                <X className="h-6 w-6" />
              ) : (
                <Menu className="h-6 w-6" />
              )}
            </button>
          </div>
        </div>
      </div>

      {/* Mobile Navigation */}
      {isMobileMenuOpen && (
        <div className="md:hidden border-t border-gray-200 dark:border-slate-800">
          <div className="px-2 pt-2 pb-3 space-y-1">
            <Link
              href="/blog"
              className="block px-3 py-2 rounded-md text-gray-700 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-slate-800 hover:text-keploy-primary transition-colors"
              onClick={() => setIsMobileMenuOpen(false)}
            >
              Blog
            </Link>
            <Link
              href="/docs"
              className="block px-3 py-2 rounded-md text-gray-700 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-slate-800 hover:text-keploy-primary transition-colors"
              onClick={() => setIsMobileMenuOpen(false)}
            >
              Documentation
            </Link>
            <Link
              href="/community"
              className="block px-3 py-2 rounded-md text-gray-700 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-slate-800 hover:text-keploy-primary transition-colors"
              onClick={() => setIsMobileMenuOpen(false)}
            >
              Community
            </Link>
          </div>
        </div>
      )}
    </nav>
  )
}
