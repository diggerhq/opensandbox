#!/bin/bash

# VirtualBox Base VM Setup Script
# This script helps you prepare an Ubuntu VM as your "pre-warmed" sandbox template

set -e

VM_NAME="${1:-ubuntu-sandbox-base}"
SNAPSHOT_NAME="ready"

echo "🔧 VirtualBox Base VM Setup"
echo "=========================="
echo ""
echo "This script will help you set up: $VM_NAME"
echo ""

# Check if VBoxManage is available
if ! command -v VBoxManage &> /dev/null; then
    echo "❌ VBoxManage not found. Is VirtualBox installed?"
    echo "   Install from: https://www.virtualbox.org/wiki/Downloads"
    exit 1
fi

echo "✅ VirtualBox detected"
echo ""

# List existing VMs
echo "📋 Existing VMs:"
VBoxManage list vms || echo "   (none)"
echo ""

# Check if target VM exists
if VBoxManage list vms | grep -q "\"$VM_NAME\""; then
    echo "✅ VM '$VM_NAME' already exists"
    
    # Check for snapshot
    if VBoxManage snapshot "$VM_NAME" list 2>/dev/null | grep -q "$SNAPSHOT_NAME"; then
        echo "✅ Snapshot '$SNAPSHOT_NAME' exists"
        echo ""
        echo "🎉 Base VM is ready to use!"
        exit 0
    else
        echo "⚠️  No '$SNAPSHOT_NAME' snapshot found"
        echo ""
        echo "To create the snapshot:"
        echo "  1. Start the VM and ensure it's fully booted"
        echo "  2. Install Guest Additions if not already installed"
        echo "  3. Shut down the VM cleanly"
        echo "  4. Run: VBoxManage snapshot \"$VM_NAME\" take \"$SNAPSHOT_NAME\""
    fi
else
    echo "❌ VM '$VM_NAME' not found"
    echo ""
    echo "📝 Setup Instructions:"
    echo ""
    echo "1. Create a new VM in VirtualBox:"
    echo "   - Name: $VM_NAME"
    echo "   - Type: Linux"
    echo "   - Version: Ubuntu (64-bit)"
    echo "   - RAM: 2048 MB (minimum)"
    echo "   - Disk: 20 GB (dynamic)"
    echo ""
    echo "2. Install Ubuntu Server (minimal recommended)"
    echo "   - Download: https://ubuntu.com/download/server"
    echo "   - Create user: sandbox / sandbox"
    echo ""
    echo "3. After Ubuntu is installed, install Guest Additions:"
    echo "   sudo apt update"
    echo "   sudo apt install -y build-essential dkms linux-headers-\$(uname -r)"
    echo "   # Insert Guest Additions CD via VirtualBox menu"
    echo "   sudo mount /dev/cdrom /mnt"
    echo "   sudo /mnt/VBoxLinuxAdditions.run"
    echo "   sudo reboot"
    echo ""
    echo "4. Configure for sandbox use:"
    echo "   sudo apt install -y curl wget git python3 nodejs npm"
    echo ""
    echo "5. Shut down the VM and take a snapshot:"
    echo "   VBoxManage snapshot \"$VM_NAME\" take \"$SNAPSHOT_NAME\""
    echo ""
fi

echo ""
echo "📖 Environment variables for the service:"
echo "   BASE_VM_NAME=$VM_NAME"
echo "   VM_USERNAME=sandbox"
echo "   VM_PASSWORD=sandbox"

