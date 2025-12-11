#!/bin/bash
# Run this INSIDE the Ubuntu VM to set up Btrfs for snapshots

set -e

echo "🔧 Setting up Btrfs for instant snapshots..."

# Install btrfs tools
sudo apt update
sudo apt install -y btrfs-progs

# Create a Btrfs filesystem on a file (easy for testing)
# In production, you'd use a dedicated partition/disk
BTRFS_IMAGE="/var/lib/btrfs-sandbox.img"
BTRFS_SIZE="10G"  # Adjust as needed

if [ ! -f "$BTRFS_IMAGE" ]; then
    echo "Creating ${BTRFS_SIZE} Btrfs image..."
    sudo truncate -s $BTRFS_SIZE $BTRFS_IMAGE
    sudo mkfs.btrfs $BTRFS_IMAGE
fi

# Create mount point
sudo mkdir -p /btrfs

# Mount the Btrfs filesystem
if ! mountpoint -q /btrfs; then
    echo "Mounting Btrfs..."
    sudo mount -o loop $BTRFS_IMAGE /btrfs
fi

# Create subvolumes for workspace and snapshots
if [ ! -d "/btrfs/workspace" ]; then
    echo "Creating workspace subvolume..."
    sudo btrfs subvolume create /btrfs/workspace
fi

if [ ! -d "/btrfs/snapshots" ]; then
    echo "Creating snapshots directory..."
    sudo mkdir -p /btrfs/snapshots
fi

# Create symlinks
sudo rm -rf /workspace /snapshots 2>/dev/null || true
sudo ln -sf /btrfs/workspace /workspace
sudo ln -sf /btrfs/snapshots /snapshots

# Set permissions
sudo chown -R $(whoami):$(whoami) /btrfs/workspace
sudo chmod 755 /btrfs/workspace /btrfs/snapshots

# Add to fstab for persistence
if ! grep -q "btrfs-sandbox" /etc/fstab; then
    echo "Adding to fstab..."
    echo "$BTRFS_IMAGE /btrfs btrfs loop 0 0" | sudo tee -a /etc/fstab
fi

# Allow user to run btrfs commands without password (for agent)
echo "Setting up sudo permissions..."
echo "$(whoami) ALL=(ALL) NOPASSWD: /usr/bin/btrfs" | sudo tee /etc/sudoers.d/btrfs-sandbox
sudo chmod 440 /etc/sudoers.d/btrfs-sandbox

echo ""
echo "✅ Btrfs setup complete!"
echo ""
echo "Directories:"
echo "  /workspace  -> Btrfs subvolume for your code"
echo "  /snapshots  -> Where snapshots are stored"
echo ""
echo "Test it:"
echo "  echo 'hello' > /workspace/test.txt"
echo "  sudo btrfs subvolume snapshot /workspace /snapshots/test1"
echo "  rm /workspace/test.txt"
echo "  sudo btrfs subvolume delete /workspace"
echo "  sudo btrfs subvolume snapshot /snapshots/test1 /workspace"
echo "  cat /workspace/test.txt  # Should show 'hello'"

