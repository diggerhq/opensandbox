export default function Login() {
  return (
    <div style={{
      display: 'flex',
      flexDirection: 'column',
      alignItems: 'center',
      justifyContent: 'center',
      height: '100vh',
      background: 'var(--bg-void)',
      position: 'relative',
      overflow: 'hidden',
    }}>
      {/* Ambient glow */}
      <div style={{
        position: 'absolute',
        width: 600, height: 600,
        borderRadius: '50%',
        background: 'radial-gradient(circle, rgba(99,102,241,0.08) 0%, transparent 70%)',
        top: '50%', left: '50%',
        transform: 'translate(-50%, -55%)',
        pointerEvents: 'none',
      }} />

      {/* Content */}
      <div style={{ position: 'relative', textAlign: 'center' }}>
        {/* Logo */}
        <div style={{
          width: 56, height: 56,
          borderRadius: 16,
          background: 'var(--gradient-primary)',
          display: 'flex', alignItems: 'center', justifyContent: 'center',
          fontSize: 20, fontWeight: 800, color: '#fff',
          fontFamily: 'var(--font-display)',
          margin: '0 auto 24px',
          boxShadow: '0 0 40px rgba(99,102,241,0.3)',
        }}>
          OS
        </div>

        <h1 style={{
          fontSize: 34,
          fontWeight: 700,
          fontFamily: 'var(--font-display)',
          letterSpacing: '-0.04em',
          color: 'var(--text-primary)',
          marginBottom: 8,
        }}>
          OpenComputer
        </h1>

        <p style={{
          fontSize: 14,
          color: 'var(--text-tertiary)',
          marginBottom: 40,
          maxWidth: 280,
          lineHeight: 1.5,
        }}>
          Sign in to manage your sandbox infrastructure
        </p>

        <a
          href="/auth/login"
          className="btn-primary"
          style={{
            display: 'inline-flex',
            padding: '14px 40px',
            fontSize: 15,
            borderRadius: 10,
            textDecoration: 'none',
          }}
        >
          Sign in
        </a>

        <div style={{
          marginTop: 48,
          fontSize: 11,
          color: 'var(--text-tertiary)',
          fontFamily: 'var(--font-mono)',
          letterSpacing: '0.04em',
        }}>
          SECURE &middot; GLOBAL &middot; FAST
        </div>
      </div>
    </div>
  )
}
