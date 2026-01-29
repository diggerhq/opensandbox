#!/bin/bash
# Run this inside the VM to set up the agent as a boot service

set -e

echo "🔧 Setting up agent as systemd service..."

# Create the systemd service file
sudo tee /etc/systemd/system/sandbox-agent.service > /dev/null << 'EOF'
[Unit]
Description=Sandbox Agent
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/home/user
ExecStart=/usr/bin/python3 /home/user/agent.py
Restart=always
RestartSec=3
Environment=PYTHONUNBUFFERED=1

[Install]
WantedBy=multi-user.target
EOF

# Enable and start the service
sudo systemctl daemon-reload
sudo systemctl enable sandbox-agent.service

echo "✅ Agent service installed!"
echo "   It will start automatically on boot."
echo "   Manual control: sudo systemctl start/stop/status sandbox-agent"

