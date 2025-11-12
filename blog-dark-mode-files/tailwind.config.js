/** @type {import('tailwindcss').Config} */
module.exports = {
  darkMode: 'class',
  content: [
    './src/pages/**/*.{js,ts,jsx,tsx,mdx}',
    './src/components/**/*.{js,ts,jsx,tsx,mdx}',
    './src/app/**/*.{js,ts,jsx,tsx,mdx}',
    './app/**/*.{js,ts,jsx,tsx,mdx}',
    './components/**/*.{js,ts,jsx,tsx,mdx}',
    './pages/**/*.{js,ts,jsx,tsx,mdx}',
  ],
  theme: {
    extend: {
      colors: {
        // Keploy brand colors
        'keploy-primary': '#2635F1',
        'keploy-primary-dark': '#3D5AFE',
        'keploy-secondary': '#FF6B6B',
        
        // Light mode
        background: '#FFFFFF',
        foreground: '#1A1A1A',
        'card-bg': '#F9FAFB',
        'card-border': '#E5E7EB',
        
        // Dark mode
        'background-dark': '#0F0F0F',
        'foreground-dark': '#FFFFFF',
        'card-bg-dark': '#1A1A1A',
        'card-border-dark': '#2D2D2D',
      },
      typography: {
        DEFAULT: {
          css: {
            color: 'var(--tw-prose-body)',
            a: {
              color: '#2635F1',
              '&:hover': {
                color: '#1a1f99',
              },
            },
          },
        },
        dark: {
          css: {
            color: 'var(--tw-prose-body)',
            a: {
              color: '#3D5AFE',
              '&:hover': {
                color: '#5B7CFF',
              },
            },
          },
        },
      },
    },
  },
  plugins: [require('@tailwindcss/typography')],
}
