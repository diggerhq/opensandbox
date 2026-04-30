package compute

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const (
	// AWS tag keys (kept consistent with the Azure pool's azure-prefixed tags).
	awsTagRole         = "opensandbox:role"
	awsTagInstanceType = "opensandbox:instance-type"
	awsTagDraining     = "opensandbox:draining"
	awsTagWorker       = "worker"
)

// EC2PoolConfig configures the EC2 compute pool.
type EC2PoolConfig struct {
	Region          string
	AccessKeyID     string // empty = use default credential chain (IAM role preferred)
	SecretAccessKey string
	AMI             string // static AMI ID; empty if SSMParameterName is set
	InstanceType    string // e.g. "c7gd.metal", "r7gd.xlarge", "m7i.large"
	SubnetID        string
	SecurityGroupID string
	KeyName         string // optional SSH key pair (debug use only)
	IAMInstanceProfile string // attached to instances; gives them Secrets Manager + S3 read
	SecretsARN         string // Secrets Manager ARN; passed to worker via WorkerSpec.SecretsRef
	SSMParameterName   string // SSM parameter for dynamic AMI ID (e.g. /opensandbox/dev/worker-ami-id)
}

// EC2Pool implements compute.Pool using AWS EC2 instances.
//
// Worker bring-up: the CP injects a WorkerSpec via SetWorkerSpec at startup.
// CreateMachine combines the spec with EC2-specific cloud-init (NVMe instance
// store mounting, IAM-role secret fetch, AMI-baked image layout) to produce
// instance user-data.
//
// Mirrors AzurePool conventions where applicable so cells in either cloud
// behave identically from the CP's perspective.
type EC2Pool struct {
	client *ec2.Client
	awsCfg aws.Config
	mu     sync.RWMutex // protects cfg.AMI + spec
	cfg    EC2PoolConfig
	spec   WorkerSpec // injected via SetWorkerSpec; copied into worker env on every CreateMachine
}

// SetWorkerSpec injects the cloud-neutral worker config. Idempotent.
// Implements compute.WorkerSpecHolder.
func (p *EC2Pool) SetWorkerSpec(spec WorkerSpec) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// EC2 worker fetches secrets via Secrets Manager + IAM, so propagate the ARN.
	if p.cfg.SecretsARN != "" && spec.SecretsRef == "" {
		spec.SecretsRef = p.cfg.SecretsARN
	}
	p.spec = spec
}

// NewEC2Pool creates an EC2 compute pool.
// If AccessKeyID is empty, uses the default AWS credential chain (IAM
// instance profile preferred, then env vars, then ~/.aws/credentials).
func NewEC2Pool(cfg EC2PoolConfig) (*EC2Pool, error) {
	var awsCfgVal aws.Config

	if cfg.AccessKeyID != "" {
		awsCfgVal = aws.Config{
			Region: cfg.Region,
			Credentials: credentials.NewStaticCredentialsProvider(
				cfg.AccessKeyID,
				cfg.SecretAccessKey,
				"",
			),
		}
	} else {
		var err error
		awsCfgVal, err = awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion(cfg.Region),
		)
		if err != nil {
			return nil, fmt.Errorf("ec2: failed to load AWS config: %w", err)
		}
	}

	return &EC2Pool{
		client: ec2.NewFromConfig(awsCfgVal),
		awsCfg: awsCfgVal,
		cfg:    cfg,
	}, nil
}

func (p *EC2Pool) CreateMachine(ctx context.Context, opts MachineOpts) (*Machine, error) {
	instanceType := p.cfg.InstanceType
	if opts.Size != "" {
		instanceType = opts.Size
	}

	p.mu.RLock()
	ami := p.cfg.AMI
	p.mu.RUnlock()
	if opts.Image != "" {
		ami = opts.Image
	}
	if ami == "" {
		return nil, fmt.Errorf("ec2: no AMI set (configure AMI or SSMParameterName)")
	}

	userData := p.buildUserData(opts)
	machineName := fmt.Sprintf("osb-worker-%s", randomSuffix())

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String(ami),
		InstanceType: ec2types.InstanceType(instanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		UserData:     aws.String(base64.StdEncoding.EncodeToString([]byte(userData))),
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeInstance,
				Tags: []ec2types.Tag{
					{Key: aws.String("Name"), Value: aws.String(machineName)},
					{Key: aws.String(awsTagRole), Value: aws.String(awsTagWorker)},
					{Key: aws.String(awsTagInstanceType), Value: aws.String(instanceType)},
				},
			},
			{
				ResourceType: ec2types.ResourceTypeVolume,
				Tags: []ec2types.Tag{
					{Key: aws.String(awsTagRole), Value: aws.String(awsTagWorker)},
				},
			},
		},
	}

	if p.cfg.SubnetID != "" {
		input.SubnetId = aws.String(p.cfg.SubnetID)
	}
	if p.cfg.SecurityGroupID != "" {
		input.SecurityGroupIds = []string{p.cfg.SecurityGroupID}
	}
	if p.cfg.KeyName != "" {
		input.KeyName = aws.String(p.cfg.KeyName)
	}
	if p.cfg.IAMInstanceProfile != "" {
		input.IamInstanceProfile = &ec2types.IamInstanceProfileSpecification{
			Name: aws.String(p.cfg.IAMInstanceProfile),
		}
	}

	result, err := p.client.RunInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("ec2: RunInstances failed: %w", err)
	}
	if len(result.Instances) == 0 {
		return nil, fmt.Errorf("ec2: no instances returned")
	}
	inst := result.Instances[0]
	return p.instanceToMachine(&inst), nil
}

func (p *EC2Pool) DestroyMachine(ctx context.Context, machineID string) error {
	_, err := p.client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{machineID},
	})
	if err != nil {
		return fmt.Errorf("ec2: TerminateInstances %s: %w", machineID, err)
	}
	return nil
}

func (p *EC2Pool) StartMachine(ctx context.Context, machineID string) error {
	_, err := p.client.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: []string{machineID},
	})
	if err != nil {
		return fmt.Errorf("ec2: StartInstances %s: %w", machineID, err)
	}
	return nil
}

func (p *EC2Pool) StopMachine(ctx context.Context, machineID string) error {
	_, err := p.client.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: []string{machineID},
	})
	if err != nil {
		return fmt.Errorf("ec2: StopInstances %s: %w", machineID, err)
	}
	return nil
}

func (p *EC2Pool) ListMachines(ctx context.Context) ([]*Machine, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + awsTagRole), Values: []string{awsTagWorker}},
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
		},
	}
	result, err := p.client.DescribeInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("ec2: DescribeInstances: %w", err)
	}
	var machines []*Machine
	for _, res := range result.Reservations {
		for _, inst := range res.Instances {
			machines = append(machines, p.instanceToMachine(&inst))
		}
	}
	return machines, nil
}

func (p *EC2Pool) HealthCheck(ctx context.Context, machineID string) error {
	result, err := p.client.DescribeInstanceStatus(ctx, &ec2.DescribeInstanceStatusInput{
		InstanceIds: []string{machineID},
	})
	if err != nil {
		return fmt.Errorf("ec2: DescribeInstanceStatus %s: %w", machineID, err)
	}
	if len(result.InstanceStatuses) == 0 {
		return fmt.Errorf("ec2: instance %s not found or not running", machineID)
	}
	st := result.InstanceStatuses[0]
	if st.InstanceStatus.Status != ec2types.SummaryStatusOk {
		return fmt.Errorf("ec2: instance %s status is %s", machineID, st.InstanceStatus.Status)
	}
	return nil
}

func (p *EC2Pool) SupportedRegions(_ context.Context) ([]string, error) {
	return []string{p.cfg.Region}, nil
}

func (p *EC2Pool) DrainMachine(ctx context.Context, machineID string) error {
	_, err := p.client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{machineID},
		Tags:      []ec2types.Tag{{Key: aws.String(awsTagDraining), Value: aws.String("true")}},
	})
	if err != nil {
		return fmt.Errorf("ec2: tag %s draining: %w", machineID, err)
	}
	return nil
}

// CleanupOrphanedResources reclaims ENIs and EBS volumes left by failed
// VM creates. Mirrors the AzurePool's NIC/disk cleanup.
//
// Satisfies the controlplane.OrphanCleaner interface.
func (p *EC2Pool) CleanupOrphanedResources(ctx context.Context) (int, error) {
	freed := 0

	// Orphaned ENIs: tagged osb-worker but unattached for >5 min.
	nicResp, err := p.client.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + awsTagRole), Values: []string{awsTagWorker}},
			{Name: aws.String("status"), Values: []string{"available"}},
		},
	})
	if err == nil {
		for _, n := range nicResp.NetworkInterfaces {
			if _, dErr := p.client.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
				NetworkInterfaceId: n.NetworkInterfaceId,
			}); dErr == nil {
				freed++
			} else {
				log.Printf("ec2: orphan ENI cleanup %s: %v", aws.ToString(n.NetworkInterfaceId), dErr)
			}
		}
	}

	// Orphaned EBS volumes: tagged osb-worker, status=available.
	volResp, err := p.client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + awsTagRole), Values: []string{awsTagWorker}},
			{Name: aws.String("status"), Values: []string{"available"}},
		},
	})
	if err == nil {
		for _, v := range volResp.Volumes {
			if _, dErr := p.client.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
				VolumeId: v.VolumeId,
			}); dErr == nil {
				freed++
			} else {
				log.Printf("ec2: orphan volume cleanup %s: %v", aws.ToString(v.VolumeId), dErr)
			}
		}
	}

	return freed, nil
}

// RefreshAMI checks SSM Parameter Store for a new AMI ID and updates the pool config.
// Returns the current AMI ID and the version string (if a sibling parameter exists).
// If SSMParameterName is not configured, returns the static AMI with no error.
//
// Satisfies the controlplane.AMIRefresher interface.
func (p *EC2Pool) RefreshAMI(ctx context.Context) (amiID string, version string, err error) {
	if p.cfg.SSMParameterName == "" {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.cfg.AMI, "", nil
	}

	ssmClient := ssm.NewFromConfig(p.awsCfg)

	result, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(p.cfg.SSMParameterName),
	})
	if err != nil {
		return "", "", fmt.Errorf("ec2: SSM GetParameter %s: %w", p.cfg.SSMParameterName, err)
	}
	newAMI := aws.ToString(result.Parameter.Value)
	if newAMI == "" {
		return "", "", fmt.Errorf("ec2: SSM parameter %s is empty", p.cfg.SSMParameterName)
	}

	// Sibling version param convention: replace last segment with worker-ami-version
	versionParam := p.cfg.SSMParameterName[:strings.LastIndex(p.cfg.SSMParameterName, "/")+1] + "worker-ami-version"
	if vResult, vErr := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(versionParam),
	}); vErr == nil {
		version = aws.ToString(vResult.Parameter.Value)
	}

	p.mu.Lock()
	if newAMI != p.cfg.AMI {
		log.Printf("ec2: AMI updated via SSM: %s -> %s (version=%s)", p.cfg.AMI, newAMI, version)
		p.cfg.AMI = newAMI
	}
	p.mu.Unlock()

	return newAMI, version, nil
}

func (p *EC2Pool) instanceToMachine(inst *ec2types.Instance) *Machine {
	id := aws.ToString(inst.InstanceId)
	status := "creating"
	if inst.State != nil {
		switch inst.State.Name {
		case ec2types.InstanceStateNameRunning:
			status = "running"
		case ec2types.InstanceStateNameStopped:
			status = "stopped"
		case ec2types.InstanceStateNamePending:
			status = "creating"
		case ec2types.InstanceStateNameTerminated, ec2types.InstanceStateNameShuttingDown:
			status = "stopped"
		}
	}

	addr := ""
	if inst.PrivateIpAddress != nil {
		addr = fmt.Sprintf("%s:9090", aws.ToString(inst.PrivateIpAddress))
	}
	httpAddr := ""
	if inst.PublicIpAddress != nil {
		httpAddr = fmt.Sprintf("http://%s:8080", aws.ToString(inst.PublicIpAddress))
	}
	region := ""
	if inst.Placement != nil {
		region = aws.ToString(inst.Placement.AvailabilityZone)
	}

	return &Machine{
		ID:       id,
		Addr:     addr,
		HTTPAddr: httpAddr,
		Region:   region,
		Status:   status,
	}
}

// buildUserData returns the EC2 instance user-data script. Combines the
// CP-supplied WorkerSpec with EC2-specific cloud-init (NVMe instance-store
// mount, AMI-baked rootfs copy, machine-id stamping).
func (p *EC2Pool) buildUserData(opts MachineOpts) string {
	_ = opts // opts.Region/Size honored at instance launch; cloud-init is cell-uniform
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\nset -euo pipefail\n\n")

	// NVMe instance store handling. Larger metal/x.gd instance families expose
	// multiple NVMe drives at /dev/nvme[1-N]n1; smaller instances rely on EBS
	// (the attached data volume). RAID 0 across instance store NVMe when present.
	sb.WriteString("# Mount data: prefer NVMe instance store (RAID 0), else first EBS data volume\n")
	sb.WriteString("if ! mountpoint -q /data 2>/dev/null; then\n")
	sb.WriteString("  mkdir -p /data\n")
	sb.WriteString("  ROOT_DEV=$(lsblk -no PKNAME $(findmnt -n -o SOURCE /) 2>/dev/null | head -1)\n")
	sb.WriteString("  NVME_DISKS=()\n")
	sb.WriteString("  for d in /dev/nvme1n1 /dev/nvme2n1 /dev/nvme3n1 /dev/nvme4n1 /dev/nvme5n1; do\n")
	sb.WriteString("    [ -b \"$d\" ] || continue\n")
	sb.WriteString("    [ \"$(basename $d)\" = \"$ROOT_DEV\" ] && continue\n")
	sb.WriteString("    NVME_DISKS+=(\"$d\")\n")
	sb.WriteString("  done\n")
	sb.WriteString("  if [ ${#NVME_DISKS[@]} -gt 1 ]; then\n")
	sb.WriteString("    mdadm --create /dev/md0 --level=0 --raid-devices=${#NVME_DISKS[@]} \"${NVME_DISKS[@]}\" --run --force\n")
	sb.WriteString("    mkfs.xfs -f -m reflink=1 /dev/md0 && mount /dev/md0 /data\n")
	sb.WriteString("  elif [ ${#NVME_DISKS[@]} -eq 1 ]; then\n")
	sb.WriteString("    mkfs.xfs -f -m reflink=1 \"${NVME_DISKS[0]}\" && mount \"${NVME_DISKS[0]}\" /data\n")
	sb.WriteString("  else\n")
	sb.WriteString("    for d in /dev/nvme1n1 /dev/sdb /dev/xvdb; do\n")
	sb.WriteString("      [ -b \"$d\" ] || continue\n")
	sb.WriteString("      mkfs.xfs -f -m reflink=1 \"$d\" && mount \"$d\" /data && break\n")
	sb.WriteString("    done\n")
	sb.WriteString("  fi\n")
	sb.WriteString("fi\n")
	sb.WriteString("mkdir -p /data/sandboxes /data/firecracker/images\n")
	sb.WriteString("# Copy AMI-baked rootfs images to data disk if not already present\n")
	sb.WriteString("if [ -d /opt/opensandbox/images ] && [ ! -f /data/firecracker/images/default.ext4 ]; then\n")
	sb.WriteString("  cp /opt/opensandbox/images/*.ext4 /data/firecracker/images/ 2>/dev/null || true\n")
	sb.WriteString("fi\n")
	sb.WriteString("if [ -d /opt/opensandbox/images/bases ] && [ ! -d /data/firecracker/images/bases ]; then\n")
	sb.WriteString("  cp -r /opt/opensandbox/images/bases /data/firecracker/images/\n")
	sb.WriteString("fi\n\n")

	// Worker env from injected WorkerSpec.
	p.mu.RLock()
	envContent := BuildWorkerEnv(p.spec)
	p.mu.RUnlock()
	if envContent != "" {
		envB64 := base64.StdEncoding.EncodeToString([]byte(envContent))
		sb.WriteString("# Write worker env (from control plane WorkerSpec)\n")
		sb.WriteString("mkdir -p /etc/opensandbox\n")
		sb.WriteString(fmt.Sprintf("echo '%s' | base64 -d > /etc/opensandbox/worker.env\n\n", envB64))

		sb.WriteString("# Patch worker identity from EC2 instance metadata (IMDSv2)\n")
		sb.WriteString("TOKEN=$(curl -s -X PUT 'http://169.254.169.254/latest/api/token' -H 'X-aws-ec2-metadata-token-ttl-seconds: 300')\n")
		sb.WriteString("MY_IP=$(curl -s -H \"X-aws-ec2-metadata-token: $TOKEN\" http://169.254.169.254/latest/meta-data/local-ipv4)\n")
		sb.WriteString("INSTANCE_ID=$(curl -s -H \"X-aws-ec2-metadata-token: $TOKEN\" http://169.254.169.254/latest/meta-data/instance-id)\n")
		sb.WriteString("WORKER_ID=\"w-aws-${INSTANCE_ID}\"\n")
		sb.WriteString("sed -i \"s|OPENSANDBOX_GRPC_ADVERTISE=.*|OPENSANDBOX_GRPC_ADVERTISE=${MY_IP}:9090|\" /etc/opensandbox/worker.env\n")
		sb.WriteString("sed -i \"s|OPENSANDBOX_HTTP_ADDR=.*|OPENSANDBOX_HTTP_ADDR=http://${MY_IP}:8081|\" /etc/opensandbox/worker.env\n")
		sb.WriteString("sed -i \"s|OPENSANDBOX_WORKER_ID=.*|OPENSANDBOX_WORKER_ID=${WORKER_ID}|\" /etc/opensandbox/worker.env\n")
		sb.WriteString("echo \"OPENSANDBOX_MACHINE_ID=${INSTANCE_ID}\" >> /etc/opensandbox/worker.env\n\n")
	}

	// Clean stale golden snapshot — must rebuild for this instance's QEMU
	sb.WriteString("rm -rf /data/sandboxes/golden-snapshot /data/sandboxes/golden\n\n")

	// Start worker
	sb.WriteString("systemctl restart opensandbox-worker\n")

	return sb.String()
}

