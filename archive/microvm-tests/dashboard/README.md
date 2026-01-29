# OpenSandbox Dashboard

A modern dashboard UI for OpenSandbox built with TanStack Router, TanStack Query, and WorkOS AuthKit.

## Tech Stack

- **Framework**: React 18 + TypeScript
- **Build Tool**: Vite
- **Routing**: TanStack Router (file-based routing)
- **Data Fetching**: TanStack Query
- **Authentication**: WorkOS AuthKit
- **Styling**: Tailwind CSS
- **Icons**: Lucide React

## Getting Started

### Prerequisites

- Node.js 18+
- npm or pnpm

### Installation

```bash
cd dashboard
npm install
```

### Configuration

1. Create a `.env` file based on `.env.example`:

```bash
cp .env.example .env
```

2. Configure your WorkOS credentials:

```env
VITE_WORKOS_CLIENT_ID=client_XXXXXXXXXXXXX
```

Get your WorkOS Client ID from the [WorkOS Dashboard](https://dashboard.workos.com).

### Development

```bash
npm run dev
```

The dashboard will be available at `http://localhost:5173`.

### Build

```bash
npm run build
```

### Preview Production Build

```bash
npm run preview
```

## Project Structure

```
dashboard/
├── public/
│   └── favicon.svg
├── src/
│   ├── routes/
│   │   ├── __root.tsx              # Root layout
│   │   ├── index.tsx               # Landing page
│   │   ├── _authenticated.tsx      # Auth layout wrapper
│   │   └── _authenticated/
│   │       └── dashboard/
│   │           ├── index.tsx       # Dashboard overview
│   │           ├── sandboxes.tsx   # Sandboxes management
│   │           └── settings.tsx    # User settings
│   ├── index.css                   # Global styles + Tailwind
│   ├── main.tsx                    # App entry point
│   └── vite-env.d.ts              # Type declarations
├── index.html
├── package.json
├── tailwind.config.js
├── tsconfig.json
└── vite.config.ts
```

## Features

- 🎨 **Beautiful UI**: Custom sand/obsidian color palette with glass morphism effects
- 🔐 **Authentication**: Secure login with WorkOS AuthKit (SSO, Social Login, etc.)
- 📱 **Responsive**: Mobile-friendly design
- ⚡ **Fast**: Vite for instant HMR, TanStack Router for type-safe routing
- 🌙 **Dark Theme**: Elegant dark mode by default

## Customization

### Colors

The color palette is defined in `tailwind.config.js`:

- `sand-*`: Warm amber/gold accent colors
- `obsidian-*`: Dark gray/slate background colors

### Fonts

- **Display**: Instrument Sans (headings, UI)
- **Mono**: JetBrains Mono (code, terminals)

## License

MIT

