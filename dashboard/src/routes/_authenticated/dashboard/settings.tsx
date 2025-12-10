import { createFileRoute } from '@tanstack/react-router'
import { useAuth } from '@workos-inc/authkit-react'
import { useState } from 'react'
import { 
  User, 
  Key, 
  Bell, 
  CreditCard,
  Shield,
  Save
} from 'lucide-react'

export const Route = createFileRoute('/_authenticated/dashboard/settings')({
  component: SettingsPage,
})

function SettingsPage() {
  const { user } = useAuth()
  const [activeTab, setActiveTab] = useState('profile')

  const tabs = [
    { id: 'profile', label: 'Profile', icon: User },
    { id: 'api-keys', label: 'API Keys', icon: Key },
    { id: 'notifications', label: 'Notifications', icon: Bell },
    { id: 'billing', label: 'Billing', icon: CreditCard },
    { id: 'security', label: 'Security', icon: Shield },
  ]

  return (
    <div className="max-w-4xl">
      <div className="mb-8">
        <h1 className="text-2xl font-mono text-white">Settings</h1>
        <p className="text-neutral-500 font-mono text-sm mt-1">
          Manage your account settings and preferences
        </p>
      </div>

      <div className="flex gap-8">
        {/* Sidebar */}
        <nav className="w-48 shrink-0 space-y-1">
          {tabs.map((tab) => (
            <button
              key={tab.id}
              onClick={() => setActiveTab(tab.id)}
              className={`w-full flex items-center gap-3 px-4 py-2.5 rounded text-left transition-all font-mono text-sm ${
                activeTab === tab.id
                  ? 'bg-neutral-800 text-white'
                  : 'text-neutral-500 hover:text-neutral-300 hover:bg-neutral-800/50'
              }`}
            >
              <tab.icon className="w-4 h-4" />
              <span>{tab.label}</span>
            </button>
          ))}
        </nav>

        {/* Content */}
        <div className="flex-1">
          {activeTab === 'profile' && <ProfileSettings user={user} />}
          {activeTab === 'api-keys' && <ApiKeysSettings />}
          {activeTab === 'notifications' && <NotificationSettings />}
          {activeTab === 'billing' && <BillingSettings />}
          {activeTab === 'security' && <SecuritySettings />}
        </div>
      </div>
    </div>
  )
}

function ProfileSettings({ user }: { user: any }) {
  return (
    <div className="border border-neutral-800 rounded-lg p-6 space-y-6">
      <div>
        <h2 className="font-mono text-white mb-4">Profile Information</h2>
        <div className="flex items-start gap-6 mb-6">
          <div className="w-20 h-20 rounded-lg bg-neutral-800 flex items-center justify-center text-neutral-400 font-mono text-2xl">
            {user?.firstName?.[0] || user?.email?.[0]?.toUpperCase() || 'U'}
          </div>
          <div>
            <button className="btn-secondary text-sm py-2">Change Avatar</button>
            <p className="font-mono text-xs text-neutral-600 mt-2">JPG, PNG or GIF. Max 2MB.</p>
          </div>
        </div>
        <div className="grid md:grid-cols-2 gap-4">
          <div>
            <label className="block font-mono text-sm text-neutral-400 mb-2">
              First Name
            </label>
            <input
              type="text"
              defaultValue={user?.firstName || ''}
              className="input-field text-sm"
            />
          </div>
          <div>
            <label className="block font-mono text-sm text-neutral-400 mb-2">
              Last Name
            </label>
            <input
              type="text"
              defaultValue={user?.lastName || ''}
              className="input-field text-sm"
            />
          </div>
          <div className="md:col-span-2">
            <label className="block font-mono text-sm text-neutral-400 mb-2">
              Email
            </label>
            <input
              type="email"
              defaultValue={user?.email || ''}
              disabled
              className="input-field text-sm opacity-60 cursor-not-allowed"
            />
            <p className="font-mono text-xs text-neutral-600 mt-1">Email is managed by your identity provider</p>
          </div>
        </div>
      </div>
      <div className="pt-4 border-t border-neutral-800 flex justify-end">
        <button className="btn-primary flex items-center gap-2 text-sm">
          <Save className="w-4 h-4" />
          Save Changes
        </button>
      </div>
    </div>
  )
}

function ApiKeysSettings() {
  const apiKeys = [
    { id: '1', name: 'Production Key', prefix: 'sk_live_...a4b2', created: 'Jan 10, 2024', lastUsed: '2 hours ago' },
    { id: '2', name: 'Development Key', prefix: 'sk_test_...f7c9', created: 'Jan 5, 2024', lastUsed: '1 day ago' },
  ]

  return (
    <div className="border border-neutral-800 rounded-lg p-6 space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="font-mono text-white">API Keys</h2>
          <p className="font-mono text-sm text-neutral-500 mt-1">
            Manage your API keys for programmatic access
          </p>
        </div>
        <button className="btn-primary text-sm py-2">
          Create New Key
        </button>
      </div>
      <div className="space-y-3">
        {apiKeys.map((key) => (
          <div
            key={key.id}
            className="p-4 rounded-lg border border-neutral-800 flex items-center justify-between"
          >
            <div>
              <p className="font-mono text-neutral-300">{key.name}</p>
              <p className="font-mono text-sm text-neutral-600 mt-1">{key.prefix}</p>
              <p className="font-mono text-xs text-neutral-700 mt-1">
                Created {key.created} · Last used {key.lastUsed}
              </p>
            </div>
            <button className="font-mono text-sm text-red-400 hover:text-red-300 transition-colors">
              Revoke
            </button>
          </div>
        ))}
      </div>
    </div>
  )
}

function NotificationSettings() {
  return (
    <div className="border border-neutral-800 rounded-lg p-6 space-y-6">
      <div>
        <h2 className="font-mono text-white">Notification Preferences</h2>
        <p className="font-mono text-sm text-neutral-500 mt-1">
          Choose what notifications you want to receive
        </p>
      </div>
      <div className="space-y-4">
        {[
          { label: 'Sandbox status changes', description: 'Get notified when your sandboxes start or stop' },
          { label: 'Weekly usage reports', description: 'Receive weekly summary of your usage' },
          { label: 'Security alerts', description: 'Important security notifications' },
          { label: 'Product updates', description: 'New features and improvements' },
        ].map((item, index) => (
          <label
            key={index}
            className="flex items-start justify-between p-4 rounded-lg border border-neutral-800 hover:border-neutral-700 transition-colors cursor-pointer"
          >
            <div>
              <p className="font-mono text-neutral-300">{item.label}</p>
              <p className="font-mono text-sm text-neutral-600 mt-1">{item.description}</p>
            </div>
            <input
              type="checkbox"
              defaultChecked={index < 3}
              className="w-5 h-5 rounded border-neutral-700 bg-neutral-900 text-white focus:ring-neutral-500"
            />
          </label>
        ))}
      </div>
    </div>
  )
}

function BillingSettings() {
  return (
    <div className="space-y-6">
      <div className="border border-neutral-800 rounded-lg p-6">
        <h2 className="font-mono text-white mb-4">Current Plan</h2>
        <div className="flex items-center justify-between p-4 rounded-lg bg-neutral-800/50 border border-neutral-700">
          <div>
            <p className="font-mono text-white">Free Tier</p>
            <p className="font-mono text-sm text-neutral-500 mt-1">
              10GB storage · 100 hours/month runtime
            </p>
          </div>
          <button className="btn-primary text-sm py-2">
            Upgrade Plan
          </button>
        </div>
      </div>
      <div className="border border-neutral-800 rounded-lg p-6">
        <h2 className="font-mono text-white mb-4">Usage This Month</h2>
        <div className="space-y-4">
          <div>
            <div className="flex justify-between font-mono text-sm mb-2">
              <span className="text-neutral-500">Storage</span>
              <span className="text-neutral-300">2.3GB / 10GB</span>
            </div>
            <div className="h-2 rounded-full bg-neutral-800 overflow-hidden">
              <div className="h-full w-[23%] bg-white rounded-full" />
            </div>
          </div>
          <div>
            <div className="flex justify-between font-mono text-sm mb-2">
              <span className="text-neutral-500">Runtime Hours</span>
              <span className="text-neutral-300">24.5h / 100h</span>
            </div>
            <div className="h-2 rounded-full bg-neutral-800 overflow-hidden">
              <div className="h-full w-[24.5%] bg-white rounded-full" />
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}

function SecuritySettings() {
  return (
    <div className="border border-neutral-800 rounded-lg p-6 space-y-6">
      <div>
        <h2 className="font-mono text-white">Security Settings</h2>
        <p className="font-mono text-sm text-neutral-500 mt-1">
          Manage your account security
        </p>
      </div>
      <div className="space-y-4">
        <div className="p-4 rounded-lg border border-neutral-800">
          <div className="flex items-center justify-between">
            <div>
              <p className="font-mono text-neutral-300">Two-Factor Authentication</p>
              <p className="font-mono text-sm text-neutral-600 mt-1">
                Add an extra layer of security to your account
              </p>
            </div>
            <button className="btn-secondary text-sm py-2">
              Enable
            </button>
          </div>
        </div>
        <div className="p-4 rounded-lg border border-neutral-800">
          <div className="flex items-center justify-between">
            <div>
              <p className="font-mono text-neutral-300">Active Sessions</p>
              <p className="font-mono text-sm text-neutral-600 mt-1">
                View and manage your active sessions
              </p>
            </div>
            <button className="font-mono text-sm text-neutral-400 hover:text-white transition-colors">
              View All
            </button>
          </div>
        </div>
        <div className="p-4 rounded-lg border border-red-500/20 bg-red-500/5">
          <div className="flex items-center justify-between">
            <div>
              <p className="font-mono text-red-400">Delete Account</p>
              <p className="font-mono text-sm text-neutral-600 mt-1">
                Permanently delete your account and all data
              </p>
            </div>
            <button className="px-4 py-2 font-mono text-sm text-red-400 border border-red-500/30 rounded hover:bg-red-500/10 transition-colors">
              Delete
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
