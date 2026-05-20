# worker-ami-aws.pkr.hcl — Build an immutable AMI for OpenSandbox workers (QEMU backend) on AWS.
#
# Mirrors deploy/packer/worker-ami.pkr.hcl (Azure variant) but targets the
# amazon-ebs builder. The setup script (`deploy/azure/setup-azure-host.sh`)
# is cloud-agnostic in practice — it installs QEMU + kernel modules + systemd
# units + Vector and never talks to Azure-specific APIs. We reuse it as-is.
#
# Differences from the Azure file:
#   - amazon-ebs source on Ubuntu 24.04 LTS x86_64 instead of azure-arm.
#   - No rootfs blob caching (the Azure variant's elaborate Azure-blob cache
#     dance was the only Azure-API touch; for the PoC we just rebuild the
#     rootfs each time, ~10min extra per bake — acceptable for low rebuild
#     frequency).
#   - Installs awscli (needed by deploy/vector/populate-vector-env.sh AWS path
#     and by the worker user-data shared-disk attach).
#   - Tags the AMI for the terraform `aws_ami` data source lookup
#     (opensandbox-role=worker, opensandbox-cloud=aws).
#
# Usage:
#   # 1. Build binaries for linux/amd64:
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.WorkerVersion=$(git rev-parse --short HEAD)" \
#     -o bin/opensandbox-worker ./cmd/worker/
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/osb-agent ./cmd/agent/
#
#   # 2. Build the rootfs context tarball:
#   tar czf /tmp/packer-rootfs-ctx.tar.gz deploy/firecracker/rootfs/ deploy/ec2/build-rootfs-docker.sh scripts/claude-agent-wrapper/
#
#   # 3. Run packer:
#   packer init deploy/packer/worker-ami-aws.pkr.hcl
#   packer build -var "worker_version=$(git rev-parse --short HEAD)" deploy/packer/worker-ami-aws.pkr.hcl
#
#   # 4. The data source in opencomputer-infra/terraform/aws/us-east-2-poc/ami.tf
#   #    picks up the new AMI on the next `tofu apply`.

packer {
  required_plugins {
    amazon = {
      version = ">= 1.3.0"
      source  = "github.com/hashicorp/amazon"
    }
  }
}

# ---------------------------------------------------------------------
# Variables
# ---------------------------------------------------------------------

variable "worker_version" {
  type        = string
  description = "Worker version (git SHA). Baked into AMI name and tags."
}

variable "agent_version" {
  type    = string
  default = ""
}

variable "region" {
  type    = string
  default = "us-east-2"
}

variable "instance_type" {
  type        = string
  default     = "c5.4xlarge"
  description = "Builder instance type. Needs enough memory for Docker rootfs build (~8GB) but doesn't need to run guest VMs, so non-metal is fine and saves ~10× vs c5.metal."
}

variable "worker_binary" {
  type    = string
  default = "bin/opensandbox-worker"
}

variable "agent_binary" {
  type    = string
  default = "bin/osb-agent"
}

variable "rootfs_context" {
  type        = string
  default     = "/tmp/packer-rootfs-ctx.tar.gz"
  description = "Pre-built tarball of rootfs + agent wrapper sources."
}

variable "vector_context" {
  type        = string
  default     = "/tmp/packer-vector-ctx.tar.gz"
  description = "Pre-built tarball of deploy/vector/ (config + populator + units). Pre-create with: tar czf /tmp/packer-vector-ctx.tar.gz deploy/vector/"
}

variable "golden_cache_bucket" {
  type        = string
  default     = ""
  description = "Optional S3 bucket to upload the bake's golden default.ext4 to (under bases/<golden_version>/). Cell-scoped — e.g. oc-aws-us-east-2-poc-golden-cache. Empty = skip upload."
}

# ---------------------------------------------------------------------
# Source
# ---------------------------------------------------------------------

source "amazon-ebs" "worker" {
  region        = var.region
  instance_type = var.instance_type
  ssh_username  = "ubuntu"
  ssh_pty       = true

  ami_name        = "opensandbox-worker-${var.worker_version}-${formatdate("YYYYMMDD-hhmm", timestamp())}"
  ami_description = "OpenSandbox worker AMI (Ubuntu 24.04, QEMU/KVM nested-virt). Built from git ${var.worker_version}."

  source_ami_filter {
    filters = {
      name                = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"
      architecture        = "x86_64"
      virtualization-type = "hvm"
      root-device-type    = "ebs"
    }
    most_recent = true
    owners      = ["099720109477"] # Canonical
  }

  ena_support   = true
  sriov_support = true

  launch_block_device_mappings {
    device_name           = "/dev/sda1"
    volume_size           = 50
    volume_type           = "gp3"
    delete_on_termination = true
  }

  # AMI tags — the terraform `aws_ami` data source in the AWS leaf filters
  # on these to pick the most-recent worker AMI for this cloud.
  tags = {
    Name                  = "opensandbox-worker-${var.worker_version}"
    "opensandbox-role"    = "worker"
    "opensandbox-cloud"   = "aws"
    "opensandbox-version" = var.worker_version
  }

  # Volume snapshot tag — propagates so the EBS snapshot underlying the AMI
  # has the same provenance metadata as the AMI itself.
  snapshot_tags = {
    "opensandbox-role"    = "worker"
    "opensandbox-cloud"   = "aws"
    "opensandbox-version" = var.worker_version
  }

  run_tags = {
    Name = "packer-opensandbox-worker-build"
  }
}

# ---------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------

build {
  sources = ["source.amazon-ebs.worker"]

  # 1. Upload pre-built binaries.
  provisioner "file" {
    source      = var.worker_binary
    destination = "/tmp/opensandbox-worker"
  }
  provisioner "file" {
    source      = var.agent_binary
    destination = "/tmp/osb-agent"
  }

  # 2. Upload rootfs build context.
  provisioner "file" {
    source      = var.rootfs_context
    destination = "/tmp/rootfs-ctx.tar.gz"
  }

  # 3. Upload the EC2 worker systemd unit (the Azure variant uses a different
  #    unit; the EC2 one was already drafted at deploy/ec2/opensandbox-worker.service).
  provisioner "file" {
    source      = "deploy/ec2/opensandbox-worker.service"
    destination = "/tmp/opensandbox-worker.service"
  }

  # 4. Upload Vector config + populator. Packer's file provisioner doesn't
  #    do recursive directory upload reliably across SSH clients, so we
  #    tar/extract the same way we do the rootfs context above. See
  #    var.vector_context for the pre-build command.
  provisioner "file" {
    source      = var.vector_context
    destination = "/tmp/vector-ctx.tar.gz"
  }
  provisioner "shell" {
    inline = [
      "mkdir -p /tmp/vector",
      "tar xzf /tmp/vector-ctx.tar.gz -C /tmp/vector --strip-components=2", # strip deploy/vector/ prefix
      "rm /tmp/vector-ctx.tar.gz",
    ]
  }

  # 5. Run the (misleadingly-named-but-cloud-agnostic) setup script. Installs
  #    QEMU, kernel modules, Docker for rootfs build, Vector, systemd units.
  provisioner "shell" {
    execute_command = "chmod +x {{ .Path }}; {{ .Vars }} sudo -E bash '{{ .Path }}'"
    script          = "deploy/azure/setup-azure-host.sh"
  }

  # 6. AWS-specific: install awscli (used by populate-vector-env.sh and by
  #    the worker user-data's shared-disk attach), then install binaries and
  #    build the golden rootfs.
  provisioner "shell" {
    execute_command = "chmod +x {{ .Path }}; {{ .Vars }} sudo -E bash '{{ .Path }}'"
    inline = [
      # awscli v2 — apt's `awscli` is v1 and missing some commands we use.
      "apt-get update -qq",
      "apt-get install -y -qq unzip",
      "curl -fsSL 'https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip' -o /tmp/awscliv2.zip",
      "cd /tmp && unzip -q awscliv2.zip && ./aws/install --update",
      "rm -rf /tmp/awscliv2.zip /tmp/aws",
      "aws --version",

      # Install worker + agent binaries.
      "mv /tmp/opensandbox-worker /usr/local/bin/opensandbox-worker",
      "chmod +x /usr/local/bin/opensandbox-worker",
      "mv /tmp/osb-agent /usr/local/bin/osb-agent",
      "chmod +x /usr/local/bin/osb-agent",

      # Install systemd unit.
      "mv /tmp/opensandbox-worker.service /etc/systemd/system/opensandbox-worker.service",
      "systemctl daemon-reload",
      "systemctl enable opensandbox-worker.service",

      # Build the golden rootfs (no caching for PoC — every bake builds from scratch).
      "mkdir -p /tmp/rootfs-ctx",
      "cd /tmp/rootfs-ctx && tar xzf /tmp/rootfs-ctx.tar.gz",
      "INPUT_HASH=$({ sha256sum /usr/local/bin/osb-agent; find /tmp/rootfs-ctx -type f | sort | xargs sha256sum; sha256sum /opt/opensandbox/guest-modules/*.ko* 2>/dev/null; } | sha256sum | awk '{print $1}')",
      "echo \"Rootfs input hash: $INPUT_HASH\"",
      "ROOTFS_UUID=$(echo \"$INPUT_HASH\" | head -c 32 | sed 's/\\(........\\)\\(....\\)\\(....\\)\\(....\\)\\(............\\)/\\1-\\2-\\3-\\4-\\5/')",
      "export ROOTFS_UUID",
      "mkdir -p /data/firecracker/images /opt/opensandbox/images",
      "cd /tmp/rootfs-ctx && bash deploy/ec2/build-rootfs-docker.sh /usr/local/bin/osb-agent /data/firecracker/images default",
      "cp /data/firecracker/images/default.ext4 /opt/opensandbox/images/default.ext4",

      # Inject guest kernel modules into rootfs.
      "GUEST_MODDIR=/opt/opensandbox/guest-modules",
      "if [ -d \"$GUEST_MODDIR\" ] && [ -f /opt/opensandbox/images/default.ext4 ]; then",
      "  MNTDIR=$(mktemp -d)",
      "  mount -o loop /opt/opensandbox/images/default.ext4 $MNTDIR",
      "  mkdir -p $MNTDIR/lib/modules/extra",
      "  cp $GUEST_MODDIR/*.ko* $MNTDIR/lib/modules/extra/ 2>/dev/null || true",
      "  umount $MNTDIR",
      "  rmdir $MNTDIR",
      "fi",

      # Stamp the golden version (hash of the final ext4) — workers read this
      # at boot to decide whether to fetch a newer golden from S3.
      "GOLDEN_VERSION=$(/usr/local/bin/opensandbox-worker golden-version /opt/opensandbox/images/default.ext4 2>/dev/null || sha256sum /opt/opensandbox/images/default.ext4 | awk '{print $1}')",
      "echo \"$GOLDEN_VERSION\" > /opt/opensandbox/images/golden-version",
      "echo \"Golden version: $GOLDEN_VERSION\"",
    ]
  }

  # 7. Optional: upload the golden to S3 so the cell's shared-disk seeder
  #    + future per-instance prefetch path can fetch it without rebuilding.
  provisioner "shell" {
    execute_command = "chmod +x {{ .Path }}; {{ .Vars }} sudo -E bash '{{ .Path }}'"
    environment_vars = [
      "GOLDEN_CACHE_BUCKET=${var.golden_cache_bucket}",
      "AWS_DEFAULT_REGION=${var.region}",
    ]
    inline = [
      "set -e",
      "if [ -z \"$GOLDEN_CACHE_BUCKET\" ]; then",
      "  echo 'No golden_cache_bucket set; skipping S3 upload (worker AMI still includes the baked golden)'",
      "  exit 0",
      "fi",
      "GOLDEN_VERSION=$(cat /opt/opensandbox/images/golden-version)",
      "S3_KEY=\"bases/$GOLDEN_VERSION/default.ext4\"",
      "echo \"Uploading default.ext4 → s3://$GOLDEN_CACHE_BUCKET/$S3_KEY (~4GB, will take a moment)\"",
      # Instance profile credentials — the bake runs on an EC2 instance and
      # picks up its role via the metadata service. If the builder role
      # doesn't have s3:PutObject on the cell's bucket, the upload fails
      # gracefully and the AMI still works (just without S3-side hydration).
      "aws s3 cp /opt/opensandbox/images/default.ext4 \"s3://$GOLDEN_CACHE_BUCKET/$S3_KEY\" || echo 'S3 upload failed — continuing (AMI golden is the only copy)'",
    ]
  }

  # 8. Write a manifest so external tooling can pin to the resulting AMI ID.
  post-processor "manifest" {
    output     = "packer-manifest-aws.json"
    strip_path = true
  }
}
