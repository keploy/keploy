import type { Metadata } from 'next'
import { Providers } from './providers'
import './globals.css'

export const metadata: Metadata = {
  title: 'Keploy Blog',
  description: 'Latest articles, tutorials, and updates from Keploy',
  viewport: 'width=device-width, initial-scale=1',
  colorScheme: 'light dark',
}

export default function RootLayout({
  children,
}: {
  children: React.ReactNode
}) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        <meta name="theme-color" content="#ffffff" media="(prefers-color-scheme: light)" />
        <meta name="theme-color" content="#0f0f0f" media="(prefers-color-scheme: dark)" />
      </head>
      <body className="bg-white dark:bg-slate-950 text-gray-900 dark:text-gray-50 transition-colors duration-200">
        <Providers>
          {children}
        </Providers>
      </body>
    </html>
  )
}
