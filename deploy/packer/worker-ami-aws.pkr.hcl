# worker-ami-aws.pkr.hcl — Packer template for AWS EC2 worker AMIs.
#
# Counterpart to worker-ami.pkr.hcl (which builds Azure managed images +
# publishes them to a Compute Gallery). This builds an AMI in EC2 and
# optionally publishes the AMI ID to SSM Parameter Store so EC2Pool's
# RefreshAMI loop picks it up automatically.
#
# Build: packer build -var "aws_region=us-east-1" deploy/packer/worker-ami-aws.pkr.hcl
#
# Inputs (most overridable via -var or env vars):
#   - aws_region              AWS region to build in
#   - source_ami_filter       Filter for the base Ubuntu AMI (default: 24.04 LTS)
#   - instance_type           Builder instance type (small is fine)
#   - ssm_param_name          If set, AMI ID is written here on success

packer {
  required_plugins {
    amazon = {
      version = ">= 1.3.0"
      source  = "github.com/hashicorp/amazon"
    }
  }
}

variable "aws_region" {
  type    = string
  default = "us-east-1"
}

variable "instance_type" {
  type    = string
  default = "t3.medium"
}

variable "ami_name_prefix" {
  type    = string
  default = "opensandbox-worker"
}

variable "worker_version" {
  type        = string
  description = "Worker version tag (e.g. git short SHA). Used in AMI name + tags."
}

variable "ssm_param_name" {
  type        = string
  description = "If set, the built AMI ID is written to this SSM parameter (e.g. /opensandbox/dev/worker-ami-id)."
  default     = ""
}

variable "guest_image_version" {
  type        = string
  description = "Hash of the canonical guest rootfs (default.ext4). Pulled from R2/S3 during AMI build."
  default     = ""
}

variable "guest_image_url" {
  type        = string
  description = "Canonical URL for the guest rootfs blob (e.g. https://r2.opensandbox.dev/golden-store/bases/{hash}/default.ext4)."
  default     = ""
}

variable "worker_binary" {
  type        = string
  default     = "bin/opensandbox-worker"
  description = "Path to the cross-compiled worker binary (linux/amd64)."
}

variable "agent_binary" {
  type        = string
  default     = "bin/osb-agent"
  description = "Path to the cross-compiled agent binary (linux/amd64)."
}

variable "kernel_path" {
  type        = string
  default     = "deploy/firecracker/vmlinux"
  description = "Path to the Linux kernel image baked into the AMI."
}

# Latest Ubuntu 24.04 LTS amd64 from Canonical's account.
data "amazon-ami" "ubuntu" {
  region      = var.aws_region
  most_recent = true
  owners      = ["099720109477"]
  filters = {
    name                = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"
    root-device-type    = "ebs"
    virtualization-type = "hvm"
    architecture        = "x86_64"
  }
}

source "amazon-ebs" "worker" {
  region        = var.aws_region
  source_ami    = data.amazon-ami.ubuntu.id
  instance_type = var.instance_type
  ssh_username  = "ubuntu"

  ami_name        = "${var.ami_name_prefix}-${var.worker_version}-${formatdate("YYYYMMDD-hhmmss", timestamp())}"
  ami_description = "OpenSandbox worker AMI — version ${var.worker_version}"

  tags = {
    Name              = "${var.ami_name_prefix}-${var.worker_version}"
    "opensandbox:version"        = var.worker_version
    "opensandbox:role"           = "worker"
    "opensandbox:guest_version"  = var.guest_image_version
  }

  # Bigger root volume so we can stage rootfs images and tools.
  launch_block_device_mappings {
    device_name           = "/dev/sda1"
    volume_size           = 30
    volume_type           = "gp3"
    delete_on_termination = true
  }
}

build {
  sources = ["source.amazon-ebs.worker"]

  # Install system packages (KVM, QEMU, mdadm, xfs tools, etc.)
  provisioner "shell" {
    inline = [
      "set -euo pipefail",
      "export DEBIAN_FRONTEND=noninteractive",
      "sudo apt-get update -qq",
      "sudo apt-get install -y -qq qemu-system-x86 qemu-utils mdadm xfsprogs e2fsprogs jq curl ca-certificates",
      "sudo systemctl disable --now systemd-resolved 2>/dev/null || true",
      "sudo mkdir -p /opt/opensandbox/images /usr/local/bin /etc/opensandbox",
    ]
  }

  # Upload worker + agent binaries
  provisioner "file" {
    source      = var.worker_binary
    destination = "/tmp/opensandbox-worker"
  }
  provisioner "file" {
    source      = var.agent_binary
    destination = "/tmp/osb-agent"
  }
  provisioner "file" {
    source      = var.kernel_path
    destination = "/tmp/vmlinux"
  }

  provisioner "shell" {
    inline = [
      "sudo install -m 0755 /tmp/opensandbox-worker /usr/local/bin/opensandbox-worker",
      "sudo install -m 0755 /tmp/osb-agent /usr/local/bin/osb-agent",
      "sudo install -m 0644 /tmp/vmlinux /opt/opensandbox/vmlinux",
    ]
  }

  # Pull the canonical guest rootfs from R2/S3 if configured.
  provisioner "shell" {
    inline = [
      "set -euo pipefail",
      "if [ -n '${var.guest_image_url}' ] && [ -n '${var.guest_image_version}' ]; then",
      "  echo 'Pulling guest rootfs ${var.guest_image_version} from ${var.guest_image_url}...'",
      "  sudo mkdir -p /opt/opensandbox/images/bases/${var.guest_image_version}",
      "  sudo curl -fsSL -o /opt/opensandbox/images/bases/${var.guest_image_version}/default.ext4 '${var.guest_image_url}'",
      "  sudo cp /opt/opensandbox/images/bases/${var.guest_image_version}/default.ext4 /opt/opensandbox/images/default.ext4",
      "fi",
    ]
  }

  # systemd unit (mirrors the one written by Azure setup-host.sh)
  provisioner "shell" {
    inline = [
      "set -euo pipefail",
      "sudo tee /etc/systemd/system/opensandbox-worker.service > /dev/null <<'UNIT'",
      "[Unit]",
      "Description=OpenSandbox Worker (QEMU backend)",
      "After=network-online.target",
      "Wants=network-online.target",
      "",
      "[Service]",
      "Type=simple",
      "ExecStartPre=/sbin/modprobe vhost_vsock",
      "EnvironmentFile=/etc/opensandbox/worker.env",
      "ExecStart=/usr/local/bin/opensandbox-worker",
      "Restart=on-failure",
      "RestartSec=5",
      "LimitNOFILE=1000000",
      "LimitNPROC=infinity",
      "KillMode=process",
      "TimeoutStopSec=300",
      "",
      "[Install]",
      "WantedBy=multi-user.target",
      "UNIT",
      "sudo systemctl daemon-reload",
      "sudo systemctl enable opensandbox-worker",
    ]
  }

  # Publish AMI ID to SSM if configured
  post-processor "shell-local" {
    only   = ["amazon-ebs.worker"]
    inline = [
      "AMI_ID=$(jq -r '.builds[-1].artifact_id' manifest.json | cut -d: -f2)",
      "if [ -n '${var.ssm_param_name}' ] && [ -n \"$AMI_ID\" ]; then",
      "  aws ssm put-parameter --region ${var.aws_region} --name '${var.ssm_param_name}' --type String --overwrite --value \"$AMI_ID\"",
      "  aws ssm put-parameter --region ${var.aws_region} --name \"$(dirname '${var.ssm_param_name}')/worker-ami-version\" --type String --overwrite --value '${var.worker_version}'",
      "  echo \"Published $AMI_ID to ${var.ssm_param_name}\"",
      "fi",
    ]
  }

  post-processor "manifest" {
    output = "manifest.json"
  }
}
