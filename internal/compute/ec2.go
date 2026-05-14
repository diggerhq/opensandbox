package compute

import (
	"context"
	"encoding/base64"
	"errors"
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

// ec2QuotaCodes are the AWS EC2 error codes that indicate a vCPU quota,
// per-account instance limit, or AZ-level capacity exhaustion. All of these
// are recoverable by retrying with a different instance type, so the
// autoscaler treats them as the ErrQuotaExceeded class.
var ec2QuotaCodes = []string{
	"VcpuLimitExceeded",
	"InstanceLimitExceeded",
	"InsufficientInstanceCapacity",
	"MaxSpotInstanceCountExceeded",
	"Unsupported", // returned when a region/AZ doesn't offer the requested type
}

func isEC2QuotaErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, code := range ec2QuotaCodes {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

func wrapEC2CreateErr(err error, format string, args ...any) error {
	wrapped := fmt.Errorf(format, args...)
	if isEC2QuotaErr(err) {
		return errors.Join(ErrQuotaExceeded, wrapped)
	}
	return wrapped
}

const (
	tagRole         = "opensandbox:role"
	tagInstanceType = "opensandbox:instance-type"
	tagDraining     = "opensandbox:draining"
)

// EC2PoolConfig configures the EC2 compute pool.
type EC2PoolConfig struct {
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	AMI             string
	InstanceType    string // "c7gd.metal", "r6gd.metal", "r7gd.metal"
	SubnetID        string
	SecurityGroupID string
	KeyName         string
	IAMInstanceProfile string // IAM instance profile name (for Secrets Manager + S3 access)
	SecretsARN         string // Secrets Manager ARN passed to worker env
	SSMParameterName   string // SSM parameter for dynamic AMI ID (e.g. /opensandbox/prod/worker-ami-id)
}

// EC2Pool implements compute.Pool using AWS EC2 instances.
type EC2Pool struct {
	client *ec2.Client
	awsCfg aws.Config
	mu     sync.RWMutex // protects cfg.AMI
	cfg    EC2PoolConfig
}

// NewEC2Pool creates an EC2 compute pool.
// If AccessKeyID is empty, uses the default AWS credential chain (IAM instance profile, env vars, etc.).
func NewEC2Pool(cfg EC2PoolConfig) (*EC2Pool, error) {
	var client *ec2.Client

	var awsCfgVal aws.Config

	if cfg.AccessKeyID != "" {
		// Static credentials
		awsCfgVal = aws.Config{
			Region: cfg.Region,
			Credentials: credentials.NewStaticCredentialsProvider(
				cfg.AccessKeyID,
				cfg.SecretAccessKey,
				"",
			),
		}
	} else {
		// IAM credential chain
		var err error
		awsCfgVal, err = awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion(cfg.Region),
		)
		if err != nil {
			return nil, fmt.Errorf("ec2: failed to load AWS config: %w", err)
		}
	}

	client = ec2.NewFromConfig(awsCfgVal)

	return &EC2Pool{
		client: client,
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

	userData := p.buildUserData(opts)

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
					{Key: aws.String("Name"), Value: aws.String("opensandbox-worker")},
					{Key: aws.String(tagRole), Value: aws.String("worker")},
					{Key: aws.String(tagInstanceType), Value: aws.String(instanceType)},
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
		return nil, wrapEC2CreateErr(err, "ec2: RunInstances failed: %w", err)
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
		return fmt.Errorf("ec2: TerminateInstances failed for %s: %w", machineID, err)
	}
	return nil
}

func (p *EC2Pool) StartMachine(ctx context.Context, machineID string) error {
	_, err := p.client.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: []string{machineID},
	})
	if err != nil {
		return fmt.Errorf("ec2: StartInstances failed for %s: %w", machineID, err)
	}
	return nil
}

func (p *EC2Pool) StopMachine(ctx context.Context, machineID string) error {
	_, err := p.client.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: []string{machineID},
	})
	if err != nil {
		return fmt.Errorf("ec2: StopInstances failed for %s: %w", machineID, err)
	}
	return nil
}

func (p *EC2Pool) ListMachines(ctx context.Context) ([]*Machine, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("tag:" + tagRole),
				Values: []string{"worker"},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"pending", "running", "stopping", "stopped"},
			},
		},
	}

	result, err := p.client.DescribeInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("ec2: DescribeInstances failed: %w", err)
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
		return fmt.Errorf("ec2: DescribeInstanceStatus failed for %s: %w", machineID, err)
	}

	if len(result.InstanceStatuses) == 0 {
		return fmt.Errorf("ec2: instance %s not found or not running", machineID)
	}

	status := result.InstanceStatuses[0]
	if status.InstanceStatus.Status != ec2types.SummaryStatusOk {
		return fmt.Errorf("ec2: instance %s status is %s", machineID, status.InstanceStatus.Status)
	}
	return nil
}

func (p *EC2Pool) SupportedRegions(_ context.Context) ([]string, error) {
	return []string{p.cfg.Region}, nil
}

func (p *EC2Pool) DrainMachine(ctx context.Context, machineID string) error {
	_, err := p.client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{machineID},
		Tags: []ec2types.Tag{
			{Key: aws.String(tagDraining), Value: aws.String("true")},
		},
	})
	if err != nil {
		return fmt.Errorf("ec2: failed to tag %s as draining: %w", machineID, err)
	}
	return nil
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

// RefreshAMI checks SSM Parameter Store for a new AMI ID and updates the pool config.
// Returns the current AMI ID and the version string (if a version parameter exists alongside).
// If SSMParameterName is not configured, returns the static AMI with no error.
func (p *EC2Pool) RefreshAMI(ctx context.Context) (amiID string, version string, err error) {
	if p.cfg.SSMParameterName == "" {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.cfg.AMI, "", nil
	}

	ssmClient := ssm.NewFromConfig(p.awsCfg)

	// Fetch AMI ID
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

	// Fetch version (convention: sibling parameter with last segment replaced by "worker-ami-version")
	// e.g. /opensandbox/prod/worker-ami-id -> /opensandbox/prod/worker-ami-version
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

func (p *EC2Pool) buildUserData(opts MachineOpts) string {
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\nset -euo pipefail\n\n")

	// Write minimal env file — secrets come from Secrets Manager via IAM role
	sb.WriteString("mkdir -p /etc/opensandbox\n")
	sb.WriteString("cat > /etc/opensandbox/worker.env << 'ENVEOF'\n")
	sb.WriteString("HOME=/root\n")
	sb.WriteString("OPENSANDBOX_MODE=worker\n")
	sb.WriteString(fmt.Sprintf("OPENSANDBOX_DATA_DIR=/data/sandboxes\n"))
	if opts.Region != "" {
		sb.WriteString(fmt.Sprintf("OPENSANDBOX_REGION=%s\n", opts.Region))
	}
	if p.cfg.SecretsARN != "" {
		sb.WriteString(fmt.Sprintf("OPENSANDBOX_SECRETS_ARN=%s\n", p.cfg.SecretsARN))
	}
	sb.WriteString("ENVEOF\n\n")

	// Mount NVMe with XFS project quotas
	sb.WriteString("# Mount NVMe instance storage with XFS project quotas\n")
	sb.WriteString("if [ -b /dev/nvme1n1 ]; then\n")
	sb.WriteString("  mkfs.xfs -f /dev/nvme1n1\n")
	sb.WriteString("  mkdir -p /data/sandboxes\n")
	sb.WriteString("  mount -o prjquota /dev/nvme1n1 /data/sandboxes\n")
	sb.WriteString("fi\n\n")

	// Start the worker service
	sb.WriteString("systemctl restart opensandbox-worker\n")

	return sb.String()
}
