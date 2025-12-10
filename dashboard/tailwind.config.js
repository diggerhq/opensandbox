/** @type {import('tailwindcss').Config} */
export default {
  content: [
    "./index.html",
    "./src/**/*.{js,ts,jsx,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        sand: {
          50: '#fdf8f3',
          100: '#f9efe3',
          200: '#f2dcc3',
          300: '#e9c59a',
          400: '#dea76d',
          500: '#d58f4a',
          600: '#c77a3f',
          700: '#a56236',
          800: '#854f32',
          900: '#6c422b',
          950: '#3a2115',
        },
        obsidian: {
          50: '#f6f6f7',
          100: '#e2e3e5',
          200: '#c5c6cb',
          300: '#a0a2aa',
          400: '#7c7f88',
          500: '#61646d',
          600: '#4d4f57',
          700: '#3f4147',
          800: '#35373b',
          900: '#1e1f22',
          950: '#121315',
        },
      },
      fontFamily: {
        mono: ['IBM Plex Mono', 'JetBrains Mono', 'Fira Code', 'monospace'],
      },
      animation: {
        'gradient-shift': 'gradient-shift 8s ease infinite',
        'float': 'float 6s ease-in-out infinite',
        'pulse-glow': 'pulse-glow 2s ease-in-out infinite',
      },
      keyframes: {
        'gradient-shift': {
          '0%, 100%': { backgroundPosition: '0% 50%' },
          '50%': { backgroundPosition: '100% 50%' },
        },
        'float': {
          '0%, 100%': { transform: 'translateY(0px)' },
          '50%': { transform: 'translateY(-10px)' },
        },
        'pulse-glow': {
          '0%, 100%': { opacity: 1, boxShadow: '0 0 20px rgba(213, 143, 74, 0.3)' },
          '50%': { opacity: 0.8, boxShadow: '0 0 40px rgba(213, 143, 74, 0.6)' },
        },
      },
    },
  },
  plugins: [],
}
