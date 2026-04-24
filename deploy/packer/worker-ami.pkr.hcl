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

variable "base_archive_account" {
  type        = string
  default     = ""
  description = "Azure storage account for archiving default.ext4 by goldenVersion hash. Empty to skip archival."
}

variable "base_archive_key" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Storage account key for the base archive. Paired with base_archive_account."
}

variable "base_archive_container" {
  type        = string
  default     = "checkpoints"
  description = "Container name for the base archive."
}

variable "prev_golden_version" {
  type        = string
  default     = ""
  description = "Previous AMI's golden version. When set, Packer downloads bases/{prev}/default.ext4 from blob storage and bakes it into the AMI at /opt/opensandbox/images/bases/{prev}/default.ext4 so forks of checkpoints pinned to the previous golden don't need a runtime blob download."
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

  # 4. Install binaries and build (or restore from cache) rootfs.
  #
  # Rootfs content-addressed caching:
  #   - Compute ROOTFS_INPUT_HASH from agent binary + all rootfs source files
  #     + guest kernel modules. Same inputs → same hash → same cached artifact
  #     → same goldenVersion at runtime. A commit that doesn't touch any
  #     rootfs input reuses the existing blob and produces a byte-identical
  #     default.ext4 across AMIs, so the worker fleet stays on one golden.
  #
  # Deterministic ext4 build:
  #   - ROOTFS_UUID is derived from the input hash and passed into
  #     build-rootfs-docker.sh, which stamps it as the ext4 UUID + hash seed.
  #     This makes mkfs.ext4 output byte-stable — the file hash matches across
  #     workers even if the cache download races multiple AMI builds.
  provisioner "shell" {
    execute_command = "chmod +x {{ .Path }}; {{ .Vars }} sudo -E bash '{{ .Path }}'"
    environment_vars = [
      "CACHE_ACCOUNT=${var.base_archive_account}",
      "CACHE_KEY=${var.base_archive_key}",
      "CACHE_CONTAINER=${var.base_archive_container}",
    ]
    inline = [
      # Install worker and agent binaries
      "mv /tmp/opensandbox-worker /usr/local/bin/opensandbox-worker",
      "chmod +x /usr/local/bin/opensandbox-worker",
      "mv /tmp/osb-agent /usr/local/bin/osb-agent",
      "chmod +x /usr/local/bin/osb-agent",

      # Extract rootfs build context
      "mkdir -p /tmp/rootfs-ctx",
      "cd /tmp/rootfs-ctx && tar xzf /tmp/rootfs-ctx.tar.gz",

      # Compute a stable hash over all rootfs inputs. Order must be
      # deterministic — use `sort` — so the same inputs always hash to the
      # same value regardless of filesystem enumeration order.
      "INPUT_HASH=$({ sha256sum /usr/local/bin/osb-agent; find /tmp/rootfs-ctx -type f | sort | xargs sha256sum; sha256sum /opt/opensandbox/guest-modules/*.ko* 2>/dev/null; } | sha256sum | awk '{print $1}')",
      "echo \"Rootfs input hash: $INPUT_HASH\"",
      "# Derive ext4 UUID from the hash (first 32 hex chars, dashed to UUID form).",
      "ROOTFS_UUID=$(echo \"$INPUT_HASH\" | head -c 32 | sed 's/\\(........\\)\\(....\\)\\(....\\)\\(....\\)\\(............\\)/\\1-\\2-\\3-\\4-\\5/')",
      "export ROOTFS_UUID",
      "# Cache key: short hash for blob path.",
      "INPUT_HASH_SHORT=$(echo \"$INPUT_HASH\" | cut -c1-16)",
      "CACHE_BLOB=\"rootfs-cache/$INPUT_HASH_SHORT/default.ext4\"",

      "mkdir -p /data/firecracker/images",

      "# Try cache download first. Fall through to fresh build on any error.",
      "CACHE_HIT=0",
      "if [ -n \"$CACHE_ACCOUNT\" ] && [ -n \"$CACHE_KEY\" ]; then",
      "  echo \"Checking rootfs cache: $CACHE_BLOB\"",
      "  CACHE_OUT=/data/firecracker/images/default.ext4",
      "  if python3 - <<PYEOF; then CACHE_HIT=1; fi",
      "import http.client, hashlib, hmac, base64, datetime, os, sys",
      "account = os.environ['CACHE_ACCOUNT']",
      "key = os.environ['CACHE_KEY']",
      "container = os.environ['CACHE_CONTAINER']",
      "blob = '$CACHE_BLOB'",
      "out_path = '$CACHE_OUT'",
      "now = datetime.datetime.utcnow().strftime('%a, %d %b %Y %H:%M:%S GMT')",
      "string_to_sign = f'GET\\n\\n\\n\\n\\n\\n\\n\\n\\n\\n\\n\\nx-ms-date:{now}\\nx-ms-version:2020-10-02\\n/{account}/{container}/{blob}'",
      "sig = base64.b64encode(hmac.new(base64.b64decode(key), string_to_sign.encode(), hashlib.sha256).digest()).decode()",
      "conn = http.client.HTTPSConnection(f'{account}.blob.core.windows.net')",
      "headers = {'x-ms-date': now, 'x-ms-version': '2020-10-02', 'Authorization': f'SharedKey {account}:{sig}'}",
      "conn.request('GET', f'/{container}/{blob}', headers=headers)",
      "resp = conn.getresponse()",
      "if resp.status != 200:",
      "    print(f'cache miss: {resp.status}'); sys.exit(1)",
      "with open(out_path, 'wb') as f:",
      "    while True:",
      "        chunk = resp.read(8 * 1024 * 1024)",
      "        if not chunk: break",
      "        f.write(chunk)",
      "print(f'cache hit: {os.path.getsize(out_path)} bytes')",
      "sys.exit(0)",
      "PYEOF",
      "fi",

      "if [ \"$CACHE_HIT\" = \"1\" ]; then",
      "  echo \"Rootfs restored from cache — skipping Docker build\"",
      "else",
      "  echo \"Cache miss — building rootfs from source with ROOTFS_UUID=$ROOTFS_UUID\"",
      "  cd /tmp/rootfs-ctx && ROOTFS_UUID=\"$ROOTFS_UUID\" bash deploy/ec2/build-rootfs-docker.sh /usr/local/bin/osb-agent /data/firecracker/images default",
      "fi",

      # Inject guest kernel modules into rootfs (applies to both cached and freshly-built images)
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

      "# On cache miss, upload the freshly built ext4 to the cache for future builds.",
      "if [ \"$CACHE_HIT\" != \"1\" ] && [ -n \"$CACHE_ACCOUNT\" ] && [ -n \"$CACHE_KEY\" ]; then",
      "  echo \"Uploading fresh rootfs to cache: $CACHE_BLOB\"",
      "  python3 - <<PYEOF || echo 'cache upload failed (non-fatal)'",
      "import http.client, hashlib, hmac, base64, datetime, os, sys",
      "account = os.environ['CACHE_ACCOUNT']",
      "key = os.environ['CACHE_KEY']",
      "container = os.environ['CACHE_CONTAINER']",
      "blob = '$CACHE_BLOB'",
      "path = '/data/firecracker/images/default.ext4'",
      "size = os.path.getsize(path)",
      "now = datetime.datetime.utcnow().strftime('%a, %d %b %Y %H:%M:%S GMT')",
      "string_to_sign = f'PUT\\n\\n\\n{size}\\n\\napplication/octet-stream\\n\\n\\n\\n\\n\\n\\nx-ms-blob-type:BlockBlob\\nx-ms-date:{now}\\nx-ms-version:2020-10-02\\n/{account}/{container}/{blob}'",
      "sig = base64.b64encode(hmac.new(base64.b64decode(key), string_to_sign.encode(), hashlib.sha256).digest()).decode()",
      "conn = http.client.HTTPSConnection(f'{account}.blob.core.windows.net')",
      "headers = {'x-ms-blob-type': 'BlockBlob', 'x-ms-date': now, 'x-ms-version': '2020-10-02', 'Content-Length': str(size), 'Content-Type': 'application/octet-stream', 'Authorization': f'SharedKey {account}:{sig}'}",
      "with open(path, 'rb') as f:",
      "    conn.request('PUT', f'/{container}/{blob}', body=f, headers=headers)",
      "    resp = conn.getresponse()",
      "    print(f'cache upload: {resp.status} {resp.reason}')",
      "    sys.exit(0 if resp.status < 400 else 1)",
      "PYEOF",
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

  # 4b. Archive base image to blob storage keyed by goldenVersion so that old
  #     checkpoints referencing this base can be rebased even after workers roll.
  provisioner "shell" {
    execute_command = "chmod +x {{ .Path }}; {{ .Vars }} sudo -E bash '{{ .Path }}'"
    environment_vars = [
      "ARCHIVE_ACCOUNT=${var.base_archive_account}",
      "ARCHIVE_KEY=${var.base_archive_key}",
      "ARCHIVE_CONTAINER=${var.base_archive_container}",
    ]
    inline = [
      "if [ -z \"$ARCHIVE_ACCOUNT\" ] || [ -z \"$ARCHIVE_KEY\" ]; then",
      "  echo 'Base archive account/key not set — skipping archival'",
      "  exit 0",
      "fi",
      "if [ ! -f /opt/opensandbox/images/default.ext4 ]; then",
      "  echo 'default.ext4 not found — skipping archival'",
      "  exit 0",
      "fi",
      "# Use the worker binary's hash function so the archive key matches what",
      "# ensureCheckpointRebased looks up at runtime.",
      "GOLDEN_VER=$(/usr/local/bin/opensandbox-worker golden-version /opt/opensandbox/images/default.ext4)",
      "echo \"Base image golden version: $GOLDEN_VER\"",
      "python3 - <<PYEOF",
      "import http.client, hashlib, hmac, base64, datetime, os, sys",
      "account = os.environ['ARCHIVE_ACCOUNT']",
      "key = os.environ['ARCHIVE_KEY']",
      "container = os.environ['ARCHIVE_CONTAINER']",
      "golden_ver = '$GOLDEN_VER'",
      "blob = f'bases/{golden_ver}/default.ext4'",
      "path = '/opt/opensandbox/images/default.ext4'",
      "",
      "# Check if already archived",
      "now = datetime.datetime.utcnow().strftime('%a, %d %b %Y %H:%M:%S GMT')",
      "string_to_sign = f'HEAD\\n\\n\\n\\n\\n\\n\\n\\n\\n\\n\\n\\nx-ms-date:{now}\\nx-ms-version:2020-10-02\\n/{account}/{container}/{blob}'",
      "signature = base64.b64encode(hmac.new(base64.b64decode(key), string_to_sign.encode(), hashlib.sha256).digest()).decode()",
      "conn = http.client.HTTPSConnection(f'{account}.blob.core.windows.net')",
      "headers = {'x-ms-date': now, 'x-ms-version': '2020-10-02', 'Authorization': f'SharedKey {account}:{signature}'}",
      "conn.request('HEAD', f'/{container}/{blob}', headers=headers)",
      "resp = conn.getresponse()",
      "resp.read()",
      "conn.close()",
      "if resp.status == 200:",
      "    print(f'Base {golden_ver} already archived')",
      "    sys.exit(0)",
      "if resp.status not in (404, 200):",
      "    print(f'HEAD check failed: {resp.status}')",
      "    sys.exit(1)",
      "",
      "# Upload",
      "size = os.path.getsize(path)",
      "print(f'Uploading {size} bytes to bases/{golden_ver}/default.ext4')",
      "now = datetime.datetime.utcnow().strftime('%a, %d %b %Y %H:%M:%S GMT')",
      "string_to_sign = f'PUT\\n\\n\\n{size}\\n\\napplication/octet-stream\\n\\n\\n\\n\\n\\n\\nx-ms-blob-type:BlockBlob\\nx-ms-date:{now}\\nx-ms-version:2020-10-02\\n/{account}/{container}/{blob}'",
      "signature = base64.b64encode(hmac.new(base64.b64decode(key), string_to_sign.encode(), hashlib.sha256).digest()).decode()",
      "conn = http.client.HTTPSConnection(f'{account}.blob.core.windows.net')",
      "headers = {",
      "    'x-ms-blob-type': 'BlockBlob',",
      "    'x-ms-date': now,",
      "    'x-ms-version': '2020-10-02',",
      "    'Content-Length': str(size),",
      "    'Content-Type': 'application/octet-stream',",
      "    'Authorization': f'SharedKey {account}:{signature}',",
      "}",
      "with open(path, 'rb') as f:",
      "    conn.request('PUT', f'/{container}/{blob}', body=f, headers=headers)",
      "    resp = conn.getresponse()",
      "    print(f'Upload: {resp.status} {resp.reason}')",
      "    if resp.status >= 400:",
      "        print(resp.read().decode())",
      "        sys.exit(1)",
      "PYEOF",
    ]
  }

  # 4c. Bake previous golden's default.ext4 into the AMI under
  #     /opt/opensandbox/images/bases/{prev}/default.ext4. At first boot the
  #     Azure custom-data script copies /opt/opensandbox/images/bases/* to
  #     /data/firecracker/images/bases/* so forks of checkpoints pinned to
  #     the previous golden skip the ~4 GB blob download.
  provisioner "shell" {
    execute_command = "chmod +x {{ .Path }}; {{ .Vars }} sudo -E bash '{{ .Path }}'"
    environment_vars = [
      "ARCHIVE_ACCOUNT=${var.base_archive_account}",
      "ARCHIVE_KEY=${var.base_archive_key}",
      "ARCHIVE_CONTAINER=${var.base_archive_container}",
      "PREV_GOLDEN=${var.prev_golden_version}",
    ]
    inline = [
      "if [ -z \"$PREV_GOLDEN\" ] || [ -z \"$ARCHIVE_ACCOUNT\" ] || [ -z \"$ARCHIVE_KEY\" ]; then",
      "  echo 'No previous golden version — skipping retention'",
      "  exit 0",
      "fi",
      "GOLDEN_VER=$(/usr/local/bin/opensandbox-worker golden-version /opt/opensandbox/images/default.ext4)",
      "if [ \"$GOLDEN_VER\" = \"$PREV_GOLDEN\" ]; then",
      "  echo 'Previous golden matches current — skipping retention (no change this build)'",
      "  exit 0",
      "fi",
      "mkdir -p /opt/opensandbox/images/bases/$PREV_GOLDEN",
      "OUT_PATH=/opt/opensandbox/images/bases/$PREV_GOLDEN/default.ext4",
      "echo \"Downloading previous base $PREV_GOLDEN → $OUT_PATH\"",
      "python3 - <<PYEOF",
      "import http.client, hashlib, hmac, base64, datetime, os, sys",
      "account = os.environ['ARCHIVE_ACCOUNT']",
      "key = os.environ['ARCHIVE_KEY']",
      "container = os.environ['ARCHIVE_CONTAINER']",
      "prev = os.environ['PREV_GOLDEN']",
      "blob = f'bases/{prev}/default.ext4'",
      "out_path = '$OUT_PATH'",
      "now = datetime.datetime.utcnow().strftime('%a, %d %b %Y %H:%M:%S GMT')",
      "string_to_sign = f'GET\\n\\n\\n\\n\\n\\n\\n\\n\\n\\n\\n\\nx-ms-date:{now}\\nx-ms-version:2020-10-02\\n/{account}/{container}/{blob}'",
      "sig = base64.b64encode(hmac.new(base64.b64decode(key), string_to_sign.encode(), hashlib.sha256).digest()).decode()",
      "conn = http.client.HTTPSConnection(f'{account}.blob.core.windows.net')",
      "headers = {'x-ms-date': now, 'x-ms-version': '2020-10-02', 'Authorization': f'SharedKey {account}:{sig}'}",
      "conn.request('GET', f'/{container}/{blob}', headers=headers)",
      "resp = conn.getresponse()",
      "if resp.status != 200:",
      "    print(f'prev-base download failed: {resp.status} — skipping retention (non-fatal)')",
      "    sys.exit(0)",
      "with open(out_path, 'wb') as f:",
      "    while True:",
      "        chunk = resp.read(8 * 1024 * 1024)",
      "        if not chunk: break",
      "        f.write(chunk)",
      "print(f'retained: {os.path.getsize(out_path)} bytes at {out_path}')",
      "PYEOF",
      # If download failed, clean up the empty directory to avoid shipping
      # a stub.
      "[ -s \"$OUT_PATH\" ] || rm -rf /opt/opensandbox/images/bases/$PREV_GOLDEN",
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
