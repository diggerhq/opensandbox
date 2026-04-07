package compute

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

const (
	azureTagRole     = "opensandbox-role"
	azureTagDraining = "opensandbox-draining"
)

// AzurePoolConfig configures the Azure compute pool.
type AzurePoolConfig struct {
	SubscriptionID     string
	ResourceGroup      string
	Region             string // e.g. "westus2"
	VMSize             string // e.g. "Standard_D16s_v5"
	ImageID            string // custom image ID or URN (e.g. "Canonical:ubuntu-24_04-lts:server:latest")
	SubnetID           string // full resource ID of the subnet
	AdminUsername      string // SSH username (default: "azureuser")
	SSHPublicKey       string // SSH public key content
	DataDiskSizeGB     int    // data disk size (default: 256)
	WorkerEnvBase64 string // base64-encoded worker.env content (injected via cloud-init)
	KeyVaultName    string // Azure Key Vault name for dynamic image ID refresh (e.g. "opensandbox-prod")
}

// AzurePool implements compute.Pool using Azure VMs.
type AzurePool struct {
	vmClient   *armcompute.VirtualMachinesClient
	diskClient *armcompute.DisksClient
	nicClient  *armnetwork.InterfacesClient
	mu         sync.RWMutex // protects cfg.ImageID
	cfg        AzurePoolConfig
	kvClient   *azsecrets.Client // Key Vault client for dynamic image refresh (nil if not configured)
}

// NewAzurePool creates an Azure compute pool using default credentials (managed identity, CLI, env vars).
func NewAzurePool(cfg AzurePoolConfig) (*AzurePool, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure: failed to get credentials: %w", err)
	}

	vmClient, err := armcompute.NewVirtualMachinesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azure: failed to create VM client: %w", err)
	}

	nicClient, err := armnetwork.NewInterfacesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azure: failed to create NIC client: %w", err)
	}

	diskClient, err := armcompute.NewDisksClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azure: failed to create disk client: %w", err)
	}

	if cfg.AdminUsername == "" {
		cfg.AdminUsername = "azureuser"
	}
	if cfg.DataDiskSizeGB == 0 {
		cfg.DataDiskSizeGB = 256
	}

	pool := &AzurePool{
		vmClient:   vmClient,
		diskClient: diskClient,
		nicClient:  nicClient,
		cfg:        cfg,
	}

	// Initialize Key Vault client for dynamic image refresh
	if cfg.KeyVaultName != "" {
		vaultURL := fmt.Sprintf("https://%s.vault.azure.net/", cfg.KeyVaultName)
		kvClient, err := azsecrets.NewClient(vaultURL, cred, nil)
		if err != nil {
			log.Printf("azure: Key Vault client failed (image refresh disabled): %v", err)
		} else {
			pool.kvClient = kvClient
			log.Printf("azure: Key Vault image refresh enabled (vault=%s)", cfg.KeyVaultName)
		}
	}

	return pool, nil
}

func (p *AzurePool) CreateMachine(ctx context.Context, opts MachineOpts) (*Machine, error) {
	vmSize := p.cfg.VMSize
	if opts.Size != "" {
		vmSize = opts.Size
	}

	vmName := fmt.Sprintf("osb-worker-%s", randomSuffix())
	nicName := vmName + "-nic"

	// Create NIC
	nicPoller, err := p.nicClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, nicName, armnetwork.Interface{
		Location: to.Ptr(p.cfg.Region),
		Tags: map[string]*string{
			azureTagRole: to.Ptr("worker"),
		},
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{
				{
					Name: to.Ptr("ipconfig1"),
					Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
						Subnet: &armnetwork.Subnet{
							ID: to.Ptr(p.cfg.SubnetID),
						},
					},
				},
			},
			EnableAcceleratedNetworking: to.Ptr(true),
		},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("azure: create NIC failed: %w", err)
	}
	nicResp, err := nicPoller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("azure: NIC poll failed: %w", err)
	}

	// Build cloud-init user data
	userData := p.buildUserData(opts)
	userDataB64 := base64.StdEncoding.EncodeToString([]byte(userData))

	// Read current image ID (may be updated by RefreshAMI)
	p.mu.RLock()
	imageID := p.cfg.ImageID
	p.mu.RUnlock()

	// Create VM
	log.Printf("azure: creating VM %s (size=%s, image=%s)", vmName, vmSize, imageID)
	vmPoller, err := p.vmClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, vmName, armcompute.VirtualMachine{
		Location: to.Ptr(p.cfg.Region),
		Tags: map[string]*string{
			"Name":       to.Ptr(vmName),
			azureTagRole: to.Ptr("worker"),
		},
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(vmSize)),
			},
			StorageProfile: p.buildStorageProfile(imageID),
			OSProfile: &armcompute.OSProfile{
				ComputerName:  to.Ptr(vmName),
				AdminUsername: to.Ptr(p.cfg.AdminUsername),
				CustomData:    to.Ptr(userDataB64),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: to.Ptr(true),
					SSH: &armcompute.SSHConfiguration{
						PublicKeys: []*armcompute.SSHPublicKey{
							{
								Path:    to.Ptr(fmt.Sprintf("/home/%s/.ssh/authorized_keys", p.cfg.AdminUsername)),
								KeyData: to.Ptr(p.cfg.SSHPublicKey),
							},
						},
					},
				},
			},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{
					{
						ID: nicResp.ID,
						Properties: &armcompute.NetworkInterfaceReferenceProperties{
							Primary: to.Ptr(true),
						},
					},
				},
			},
		},
	}, nil)
	if err != nil {
		log.Printf("azure: VM %s BeginCreateOrUpdate error detail: %+v", vmName, err)
		// Clean up orphaned NIC
		go func() {
			cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if delPoller, delErr := p.nicClient.BeginDelete(cleanCtx, p.cfg.ResourceGroup, nicName, nil); delErr == nil {
				delPoller.PollUntilDone(cleanCtx, nil)
				log.Printf("azure: cleaned up orphaned NIC %s", nicName)
			}
		}()
		return nil, fmt.Errorf("azure: create VM %s failed: %w", vmName, err)
	}
	vmResp, err := vmPoller.PollUntilDone(ctx, nil)
	if err != nil {
		// Clean up orphaned NIC on poll failure too
		go func() {
			cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if delPoller, delErr := p.nicClient.BeginDelete(cleanCtx, p.cfg.ResourceGroup, nicName, nil); delErr == nil {
				delPoller.PollUntilDone(cleanCtx, nil)
				log.Printf("azure: cleaned up orphaned NIC %s after poll failure", nicName)
			}
		}()
		return nil, fmt.Errorf("azure: VM %s poll failed: %w", vmName, err)
	}
	log.Printf("azure: VM %s created successfully", vmName)

	return p.vmToMachine(&vmResp.VirtualMachine, &nicResp.Interface), nil
}

func (p *AzurePool) DestroyMachine(ctx context.Context, machineID string) error {
	log.Printf("azure: destroying VM %s (+ disks + NIC)", machineID)

	// Get VM details before deleting (need disk names for cleanup)
	var diskNames []string
	vm, err := p.vmClient.Get(ctx, p.cfg.ResourceGroup, machineID, nil)
	if err == nil && vm.Properties != nil && vm.Properties.StorageProfile != nil {
		if vm.Properties.StorageProfile.OSDisk != nil && vm.Properties.StorageProfile.OSDisk.ManagedDisk != nil {
			if id := vm.Properties.StorageProfile.OSDisk.ManagedDisk.ID; id != nil {
				parts := strings.Split(*id, "/")
				diskNames = append(diskNames, parts[len(parts)-1])
			}
		}
		for _, dd := range vm.Properties.StorageProfile.DataDisks {
			if dd.ManagedDisk != nil && dd.ManagedDisk.ID != nil {
				parts := strings.Split(*dd.ManagedDisk.ID, "/")
				diskNames = append(diskNames, parts[len(parts)-1])
			}
		}
	}

	// Delete VM
	vmPoller, err := p.vmClient.BeginDelete(ctx, p.cfg.ResourceGroup, machineID, nil)
	if err != nil {
		return fmt.Errorf("azure: delete VM %s failed: %w", machineID, err)
	}
	if _, err := vmPoller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("azure: delete VM %s poll failed: %w", machineID, err)
	}

	// Clean up NIC
	nicName := machineID + "-nic"
	nicPoller, err := p.nicClient.BeginDelete(ctx, p.cfg.ResourceGroup, nicName, nil)
	if err == nil {
		nicPoller.PollUntilDone(ctx, nil)
	}

	// Clean up disks
	for _, diskName := range diskNames {
		diskPoller, err := p.diskClient.BeginDelete(ctx, p.cfg.ResourceGroup, diskName, nil)
		if err == nil {
			diskPoller.PollUntilDone(ctx, nil)
			log.Printf("azure: deleted disk %s", diskName)
		}
	}

	log.Printf("azure: VM %s destroyed (+ %d disks + NIC)", machineID, len(diskNames))
	return nil
}

func (p *AzurePool) StartMachine(ctx context.Context, machineID string) error {
	poller, err := p.vmClient.BeginStart(ctx, p.cfg.ResourceGroup, machineID, nil)
	if err != nil {
		return fmt.Errorf("azure: start VM %s failed: %w", machineID, err)
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

func (p *AzurePool) StopMachine(ctx context.Context, machineID string) error {
	poller, err := p.vmClient.BeginDeallocate(ctx, p.cfg.ResourceGroup, machineID, nil)
	if err != nil {
		return fmt.Errorf("azure: stop VM %s failed: %w", machineID, err)
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

func (p *AzurePool) ListMachines(ctx context.Context) ([]*Machine, error) {
	pager := p.vmClient.NewListPager(p.cfg.ResourceGroup, nil)

	var machines []*Machine
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure: list VMs failed: %w", err)
		}
		for _, vm := range page.Value {
			if vm.Tags == nil || vm.Tags[azureTagRole] == nil || *vm.Tags[azureTagRole] != "worker" {
				continue
			}
			machines = append(machines, p.vmToMachine(vm, nil))
		}
	}
	return machines, nil
}

func (p *AzurePool) HealthCheck(ctx context.Context, machineID string) error {
	vm, err := p.vmClient.Get(ctx, p.cfg.ResourceGroup, machineID, &armcompute.VirtualMachinesClientGetOptions{
		Expand: to.Ptr(armcompute.InstanceViewTypesInstanceView),
	})
	if err != nil {
		return fmt.Errorf("azure: get VM %s failed: %w", machineID, err)
	}
	if vm.Properties == nil || vm.Properties.InstanceView == nil {
		return fmt.Errorf("azure: no instance view for %s", machineID)
	}
	for _, s := range vm.Properties.InstanceView.Statuses {
		if s.Code != nil && *s.Code == "PowerState/running" {
			return nil
		}
	}
	return fmt.Errorf("azure: VM %s is not running", machineID)
}

func (p *AzurePool) SupportedRegions(_ context.Context) ([]string, error) {
	return []string{p.cfg.Region}, nil
}

func (p *AzurePool) DrainMachine(ctx context.Context, machineID string) error {
	vm, err := p.vmClient.Get(ctx, p.cfg.ResourceGroup, machineID, nil)
	if err != nil {
		return fmt.Errorf("azure: get VM %s for drain: %w", machineID, err)
	}
	if vm.Tags == nil {
		vm.Tags = map[string]*string{}
	}
	vm.Tags[azureTagDraining] = to.Ptr("true")

	poller, err := p.vmClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, machineID, vm.VirtualMachine, nil)
	if err != nil {
		return fmt.Errorf("azure: tag VM %s as draining: %w", machineID, err)
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

// CleanupOrphanedResources finds and deletes NICs not attached to any VM.
// These accumulate when VM creation fails partway through.
func (p *AzurePool) CleanupOrphanedResources(ctx context.Context) (int, error) {
	cleaned := 0
	pager := p.nicClient.NewListPager(p.cfg.ResourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return cleaned, fmt.Errorf("azure: list NICs: %w", err)
		}
		for _, nic := range page.Value {
			if nic.Properties == nil || nic.Properties.VirtualMachine != nil {
				continue // attached to a VM, skip
			}
			name := ""
			if nic.Name != nil {
				name = *nic.Name
			}
			if !strings.HasPrefix(name, "osb-worker-") {
				continue // not ours
			}
			log.Printf("azure: cleaning orphaned NIC %s", name)
			poller, err := p.nicClient.BeginDelete(ctx, p.cfg.ResourceGroup, name, nil)
			if err != nil {
				log.Printf("azure: failed to delete orphaned NIC %s: %v", name, err)
				continue
			}
			if _, err := poller.PollUntilDone(ctx, nil); err != nil {
				log.Printf("azure: orphaned NIC %s delete poll failed: %v", name, err)
			} else {
				cleaned++
			}
		}
	}
	return cleaned, nil
}

// RefreshAMI checks Azure Key Vault for a new worker image ID and version.
// Satisfies the controlplane.AMIRefresher interface.
// If Key Vault is not configured, returns the static image ID with no error.
func (p *AzurePool) RefreshAMI(ctx context.Context) (imageID string, version string, err error) {
	if p.kvClient == nil {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.cfg.ImageID, "", nil
	}

	// Fetch image ID from Key Vault
	resp, err := p.kvClient.GetSecret(ctx, "worker-image-id", "", nil)
	if err != nil {
		return "", "", fmt.Errorf("azure: Key Vault get worker-image-id: %w", err)
	}
	if resp.Value == nil || *resp.Value == "" {
		return "", "", fmt.Errorf("azure: Key Vault worker-image-id is empty")
	}
	newImageID := *resp.Value

	// Fetch version
	if vResp, vErr := p.kvClient.GetSecret(ctx, "worker-image-version", "", nil); vErr == nil && vResp.Value != nil {
		version = *vResp.Value
	}

	p.mu.Lock()
	if newImageID != p.cfg.ImageID {
		log.Printf("azure: image updated via Key Vault: %s -> %s (version=%s)", p.cfg.ImageID, newImageID, version)
		p.cfg.ImageID = newImageID
	}
	p.mu.Unlock()

	return newImageID, version, nil
}

// buildStorageProfile creates the storage profile, handling the difference between
// custom images (which already define OS disk) and marketplace images (which need explicit OS disk config).
func (p *AzurePool) buildStorageProfile(imageID string) *armcompute.StorageProfile {
	isCustomImage := strings.HasPrefix(imageID, "/")

	profile := &armcompute.StorageProfile{
		ImageReference: p.parseImageRef(imageID),
	}

	// Custom images already define OS disk and may include data disks.
	// Don't override any disk config — use what's in the image.
	if !isCustomImage {
		profile.OSDisk = &armcompute.OSDisk{
			CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
			ManagedDisk: &armcompute.ManagedDiskParameters{
				StorageAccountType: to.Ptr(armcompute.StorageAccountTypesPremiumLRS),
			},
			DiskSizeGB: to.Ptr(int32(64)),
		}
		profile.DataDisks = []*armcompute.DataDisk{
			{
				Lun:          to.Ptr(int32(0)),
				CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesEmpty),
				DiskSizeGB:   to.Ptr(int32(p.cfg.DataDiskSizeGB)),
				ManagedDisk: &armcompute.ManagedDiskParameters{
					StorageAccountType: to.Ptr(armcompute.StorageAccountTypesPremiumLRS),
				},
			},
		}
	}

	return profile
}

// parseImageRef parses the image reference. Supports:
//   - Full resource ID: /subscriptions/.../images/my-image
//   - URN: Publisher:Offer:SKU:Version (e.g. Canonical:ubuntu-24_04-lts:server:latest)
func (p *AzurePool) parseImageRef(img string) *armcompute.ImageReference {
	if strings.HasPrefix(img, "/") {
		return &armcompute.ImageReference{ID: to.Ptr(img)}
	}
	parts := strings.SplitN(img, ":", 4)
	if len(parts) == 4 {
		return &armcompute.ImageReference{
			Publisher: to.Ptr(parts[0]),
			Offer:    to.Ptr(parts[1]),
			SKU:      to.Ptr(parts[2]),
			Version:  to.Ptr(parts[3]),
		}
	}
	// Fallback: treat as ID
	return &armcompute.ImageReference{ID: to.Ptr(img)}
}

func (p *AzurePool) vmToMachine(vm *armcompute.VirtualMachine, nic *armnetwork.Interface) *Machine {
	name := ""
	if vm.Name != nil {
		name = *vm.Name
	}

	status := "creating"
	if vm.Properties != nil && vm.Properties.InstanceView != nil {
		for _, s := range vm.Properties.InstanceView.Statuses {
			if s.Code == nil {
				continue
			}
			switch *s.Code {
			case "PowerState/running":
				status = "running"
			case "PowerState/stopped", "PowerState/deallocated":
				status = "stopped"
			}
		}
	}

	// Get private IP from NIC
	addr := ""
	if nic != nil && nic.Properties != nil {
		for _, ipCfg := range nic.Properties.IPConfigurations {
			if ipCfg.Properties != nil && ipCfg.Properties.PrivateIPAddress != nil {
				addr = fmt.Sprintf("%s:9090", *ipCfg.Properties.PrivateIPAddress)
				break
			}
		}
	}

	return &Machine{
		ID:     name,
		Addr:   addr,
		Region: p.cfg.Region,
		Status: status,
	}
}

func (p *AzurePool) buildUserData(opts MachineOpts) string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\nset -euo pipefail\n\n")

	// Mount data/temp disk as XFS with reflink (required for QEMU snapshot copies)
	// v6 VMs use NVMe paths, v5 uses SCSI paths. Try both.
	sb.WriteString("# Mount data disk (XFS with reflink)\n")
	sb.WriteString("if ! mountpoint -q /data 2>/dev/null; then\n")
	sb.WriteString("  mkdir -p /data\n")
	sb.WriteString("  DISK=\"\"\n")
	sb.WriteString("  # Try NVMe temp disks (v6 VMs), then SCSI data disks (v5 VMs), then SCSI temp disk\n")
	sb.WriteString("  for d in /dev/nvme1n1 /dev/nvme2n1 /dev/nvme0n1 /dev/sdc /dev/sdd /dev/sdb; do\n")
	sb.WriteString("    [ -b \"$d\" ] || continue\n")
	sb.WriteString("    # Skip the OS disk\n")
	sb.WriteString("    ROOT_DEV=$(lsblk -no PKNAME $(findmnt -n -o SOURCE /) 2>/dev/null | head -1)\n")
	sb.WriteString("    [ \"$(basename $d)\" = \"$ROOT_DEV\" ] && continue\n")
	sb.WriteString("    DISK=\"$d\"; break\n")
	sb.WriteString("  done\n")
	sb.WriteString("  if [ -n \"$DISK\" ]; then\n")
	sb.WriteString("    if blkid \"$DISK\" 2>/dev/null | grep -q xfs; then\n")
	sb.WriteString("      mount \"$DISK\" /data\n")
	sb.WriteString("    else\n")
	sb.WriteString("      mkfs.xfs -f -m reflink=1 \"$DISK\" && mount \"$DISK\" /data\n")
	sb.WriteString("    fi\n")
	sb.WriteString("  fi\n")
	sb.WriteString("fi\n")
	sb.WriteString("mkdir -p /data/sandboxes /data/firecracker/images\n")
	sb.WriteString("# Copy rootfs images from OS disk to data disk (NVMe mount may overlay /data)\n")
	sb.WriteString("if [ -d /opt/opensandbox/images ] && [ ! -f /data/firecracker/images/default.ext4 ]; then\n")
	sb.WriteString("  cp /opt/opensandbox/images/*.ext4 /data/firecracker/images/ 2>/dev/null || true\n")
	sb.WriteString("  echo 'Copied rootfs images from /opt/opensandbox/images to /data/firecracker/images'\n")
	sb.WriteString("fi\n\n")

	// Write worker env file from base64-encoded config
	if p.cfg.WorkerEnvBase64 != "" {
		sb.WriteString("# Write worker env (from control plane config)\n")
		sb.WriteString("mkdir -p /etc/opensandbox\n")
		sb.WriteString(fmt.Sprintf("echo '%s' | base64 -d > /etc/opensandbox/worker.env\n\n", p.cfg.WorkerEnvBase64))

		// Patch in the VM's own private IP and identity
		sb.WriteString("# Patch worker identity with this VM's private IP and hostname\n")
		sb.WriteString("MY_IP=$(hostname -I | awk '{print $1}')\n")
		sb.WriteString("VM_NAME=$(hostname)\n")
		sb.WriteString("WORKER_ID=\"w-azure-${VM_NAME}\"\n")
		sb.WriteString("sed -i \"s|OPENSANDBOX_GRPC_ADVERTISE=.*|OPENSANDBOX_GRPC_ADVERTISE=${MY_IP}:9090|\" /etc/opensandbox/worker.env\n")
		sb.WriteString("sed -i \"s|OPENSANDBOX_HTTP_ADDR=.*|OPENSANDBOX_HTTP_ADDR=http://${MY_IP}:8081|\" /etc/opensandbox/worker.env\n")
		sb.WriteString("sed -i \"s|OPENSANDBOX_WORKER_ID=.*|OPENSANDBOX_WORKER_ID=${WORKER_ID}|\" /etc/opensandbox/worker.env\n")
		sb.WriteString("# Machine ID = VM name (used by scaler for drain/destroy)\n")
		sb.WriteString("echo \"OPENSANDBOX_MACHINE_ID=${VM_NAME}\" >> /etc/opensandbox/worker.env\n\n")
	}

	// Clean stale golden snapshot from image (must rebuild on this VM)
	sb.WriteString("# Clean stale golden snapshot — must rebuild for this VM's QEMU instance\n")
	sb.WriteString("rm -rf /data/sandboxes/golden-snapshot /data/sandboxes/golden\n\n")

	// Start worker
	sb.WriteString("systemctl restart opensandbox-worker\n")

	return sb.String()
}

// randomSuffix generates an 8-char hex suffix for VM names.
func randomSuffix() string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
