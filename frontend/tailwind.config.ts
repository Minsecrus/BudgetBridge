import type { Config } from "tailwindcss";

export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        ink: { 950: '#0a0a12', 900: '#0e0e18', 800: '#15151f' },
        glass: { DEFAULT: 'rgba(255,255,255,0.05)', border: 'rgba(255,255,255,0.12)' },
        aurora: { violet: '#7c3aed', pink: '#ec4899', cyan: '#22d3ee' },
      },
      backgroundImage: {
        'aurora': 'radial-gradient(circle at 20% 15%, rgba(124,58,237,.30), transparent 45%), radial-gradient(circle at 80% 25%, rgba(236,72,153,.25), transparent 45%), radial-gradient(circle at 55% 95%, rgba(34,211,238,.22), transparent 50%)',
      },
      boxShadow: {
        'glow-violet': '0 0 24px rgba(124,58,237,.35)',
        'glow-cyan': '0 0 24px rgba(34,211,238,.35)',
        'glow-pink': '0 0 24px rgba(236,72,153,.35)',
        'glass': '0 8px 32px rgba(0,0,0,.37)',
      },
      keyframes: {
        'aurora-float': { '0%,100%': { transform: 'translate3d(0,0,0) scale(1)' }, '50%': { transform: 'translate3d(0,-2%,0) scale(1.05)' } },
        'pulse-glow': { '0%,100%': { opacity: '1' }, '50%': { opacity: '.45' } },
        shimmer: { '100%': { transform: 'translateX(100%)' } },
      },
      animation: {
        'aurora-float': 'aurora-float 14s ease-in-out infinite',
        'pulse-glow': 'pulse-glow 1.8s ease-in-out infinite',
        shimmer: 'shimmer 1.5s infinite',
      },
    },
  },
  plugins: [],
} satisfies Config;
