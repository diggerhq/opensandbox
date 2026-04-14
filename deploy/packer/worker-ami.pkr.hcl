# worker-image.pkr.hcl — Build an immutable Azure Managed Image for OpenSandbox workers (QEMU backend).
#
# The image includes everything a worker needs: QEMU, guest kernel, worker + agent
# binaries, and pre-built rootfs images. At boot, only instance-specific config
# (identity, secrets, worker env) is injected via cloud-init.
#
# Usage:
#   # Build binaries first (x86_64 for Azure):
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.WorkerVersion=$(git rev-parse --short HEAD)" \
#     -o bin/opensandbox-worker ./cmd/worker/
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/osb-agent ./cmd/agent/
#
#   # Then build the image:
#   packer init deploy/packer/worker-image.pkr.hcl
#   packer build -var "worker_version=$(git rev-parse --short HEAD)" \
#     -var "subscription_id=YOUR_SUB" -var "resource_group=YOUR_RG" \
#     deploy/packer/worker-image.pkr.hcl

packer {
  required_plugins {
    azure = {
      version = ">= 2.1.0"
      source  = "github.com/hashicorp/azure"
    }
  }
}

# ---------------------------------------------------------------------
# Variables
# ---------------------------------------------------------------------

variable "worker_version" {
  type        = string
  description = "Worker version (git SHA). Baked into image name and tags."
}

variable "agent_version" {
  type        = string
  default     = ""
  description = "Agent version (git SHA). Defaults to worker_version if empty."
}

variable "subscription_id" {
  type        = string
  description = "Azure subscription ID."
}

variable "resource_group" {
  type        = string
  description = "Resource group for the managed image."
}

variable "location" {
  type    = string
  default = "westus2"
}

variable "vm_size" {
  type        = string
  default     = "Standard_D4ads_v7"
  description = "Builder VM size. Must match the autoscaled worker VM family for disk controller compatibility."
}

variable "image_name_prefix" {
  type    = string
  default = "opensandbox-worker"
}

variable "gallery_name" {
  type        = string
  default     = "opensandbox_gallery"
  description = "Azure Compute Gallery name for NVMe-compatible images."
}

variable "image_version_patch" {
  type        = string
  default     = "0"
  description = "Patch version for gallery image (integer). Set by CI to a unique number."
}

variable "worker_binary" {
  type        = string
  default     = "bin/opensandbox-worker"
  description = "Path to the pre-built worker binary (amd64 Linux)."
}

variable "agent_binary" {
  type        = string
  default     = "bin/osb-agent"
  description = "Path to the pre-built agent binary (amd64 Linux)."
}

# ---------------------------------------------------------------------
# Source: Ubuntu 24.04 x86_64 on Azure
# ---------------------------------------------------------------------

source "azure-arm" "worker" {
  subscription_id = var.subscription_id
  location        = var.location

  # Use managed identity or Azure CLI credentials
  use_azure_cli_auth = true

  # Base image: Ubuntu 24.04 LTS
  image_publisher = "Canonical"
  image_offer     = "ubuntu-24_04-lts"
  image_sku       = "server"

  os_type         = "Linux"
  vm_size         = var.vm_size
  ssh_username    = "packer"

  # Output: Managed Image (required as intermediate for gallery publish)
  managed_image_name                = "${var.image_name_prefix}-${var.worker_version}"
  managed_image_resource_group_name = var.resource_group

  # Also publish to Azure Compute Gallery for NVMe/v7 VM compatibility
  shared_image_gallery_destination {
    subscription   = var.subscription_id
    resource_group = var.resource_group
    gallery_name   = var.gallery_name
    image_name     = "osb-worker-v7"
    image_version  = "1.0.${var.image_version_patch}"
    replication_regions = [var.location]
  }

  azure_tags = {
    "opensandbox-role"    = "worker"
    "opensandbox-version" = var.worker_version
  }
}

# ---------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------

build {
  sources = ["source.azure-arm.worker"]

  # 1. Upload pre-built binaries
  provisioner "file" {
    source      = var.worker_binary
    destination = "/tmp/opensandbox-worker"
  }

  provisioner "file" {
    source      = var.agent_binary
    destination = "/tmp/osb-agent"
  }

  # 2. Upload rootfs build context as tarball
  #    Pre-create with: tar czf /tmp/packer-rootfs-ctx.tar.gz deploy/firecracker/rootfs/ deploy/ec2/build-rootfs-docker.sh scripts/claude-agent-wrapper/
  provisioner "file" {
    source      = "/tmp/packer-rootfs-ctx.tar.gz"
    destination = "/tmp/rootfs-ctx.tar.gz"
  }

  # 3. Run the Azure setup script (installs QEMU, kernel, system deps, systemd units)
  provisioner "shell" {
    execute_command = "chmod +x {{ .Path }}; {{ .Vars }} sudo -E bash '{{ .Path }}'"
    script          = "deploy/azure/setup-azure-host.sh"
  }

  # 4. Install binaries and build rootfs
  provisioner "shell" {
    execute_command = "chmod +x {{ .Path }}; {{ .Vars }} sudo -E bash '{{ .Path }}'"
    inline = [
      # Install worker and agent binaries
      "mv /tmp/opensandbox-worker /usr/local/bin/opensandbox-worker",
      "chmod +x /usr/local/bin/opensandbox-worker",
      "mv /tmp/osb-agent /usr/local/bin/osb-agent",
      "chmod +x /usr/local/bin/osb-agent",

      # Extract rootfs build context
      "mkdir -p /tmp/rootfs-ctx",
      "cd /tmp/rootfs-ctx && tar xzf /tmp/rootfs-ctx.tar.gz",

      # Build rootfs images (Docker was installed by setup-azure-host.sh)
      "mkdir -p /data/firecracker/images",
      "cd /tmp/rootfs-ctx && bash deploy/ec2/build-rootfs-docker.sh /usr/local/bin/osb-agent /data/firecracker/images default",

      # Inject guest kernel modules into rootfs
      "GUEST_MODDIR=/opt/opensandbox/guest-modules",
      "if [ -d \"$GUEST_MODDIR\" ] && [ -f /data/firecracker/images/default.ext4 ]; then",
      "  MNTDIR=$(mktemp -d)",
      "  mount -o loop /data/firecracker/images/default.ext4 $MNTDIR",
      "  mkdir -p $MNTDIR/lib/modules/extra",
      "  cp $GUEST_MODDIR/*.ko* $MNTDIR/lib/modules/extra/ 2>/dev/null || true",
      "  umount $MNTDIR",
      "  rmdir $MNTDIR",
      "  echo 'Guest kernel modules injected into rootfs'",
      "fi",

      # Save rootfs to /opt (survives NVMe mount overlay on /data)
      "mkdir -p /opt/opensandbox/images",
      "cp /data/firecracker/images/*.ext4 /opt/opensandbox/images/",

      # Cleanup build artifacts
      "rm -rf /tmp/rootfs-ctx /tmp/rootfs-ctx.tar.gz",
      "apt-get clean",
      "rm -rf /var/lib/apt/lists/*",

      # Remove any stale golden snapshot (must rebuild per-instance at first boot)
      "rm -rf /data/sandboxes/golden-snapshot /data/sandboxes/golden 2>/dev/null || true",
    ]
  }

  # 5. Deprovision for Azure image capture
  provisioner "shell" {
    execute_command = "chmod +x {{ .Path }}; {{ .Vars }} sudo -E bash '{{ .Path }}'"
    inline = [
      "/usr/sbin/waagent -force -deprovision+user && export HISTSIZE=0 && sync",
    ]
  }
}
