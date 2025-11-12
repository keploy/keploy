'use client'

import { ThemeProvider } from 'next-themes'
import { ReactNode } from 'react'

export function Providers({ children }: { children: ReactNode }) {
  return (
    <ThemeProvider
      attribute="class"
      defaultTheme="light"
      enableSystem
      storageKey="keploy-blog-theme"
      themes={['light', 'dark']}
      forcedTheme={undefined}
      enableColorSchemeChange
      disableTransitionOnChange={false}
    >
      {children}
    </ThemeProvider>
  )
}
