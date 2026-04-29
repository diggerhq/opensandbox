import { Routes, Route, Navigate } from 'react-router-dom'
import { AuthProvider } from './hooks/useAuth'
import ProtectedRoute from './components/ProtectedRoute'
import Layout from './components/Layout'
import Login from './pages/Login'
import Dashboard from './pages/Dashboard'
import Sessions from './pages/Sessions'
import Agents from './pages/Agents'
import APIKeys from './pages/APIKeys'
import Checkpoints from './pages/Checkpoints'
import Templates from './pages/Templates'
import Settings from './pages/Settings'
import Billing from './pages/Billing'
import SessionDetail from './pages/SessionDetail'

export default function App() {
  return (
    <AuthProvider>
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route element={<ProtectedRoute />}>
          <Route element={<Layout />}>
            <Route index element={<Dashboard />} />
            <Route path="sessions" element={<Sessions />} />
            <Route path="sessions/:sandboxId" element={<SessionDetail />} />
            <Route path="agents" element={<Agents />} />
            <Route path="checkpoints" element={<Checkpoints />} />
            <Route path="templates" element={<Templates />} />
            <Route path="api-keys" element={<APIKeys />} />
            <Route path="billing" element={<Billing />} />
            <Route path="settings" element={<Settings />} />
          </Route>
        </Route>
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </AuthProvider>
  )
}
