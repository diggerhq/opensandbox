package compute

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

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
}

// EC2Pool implements compute.Pool using AWS EC2 instances.
type EC2Pool struct {
	client *ec2.Client
	cfg    EC2PoolConfig
}

// NewEC2Pool creates an EC2 compute pool.
// If AccessKeyID is empty, uses the default AWS credential chain (IAM instance profile, env vars, etc.).
func NewEC2Pool(cfg EC2PoolConfig) (*EC2Pool, error) {
	var client *ec2.Client

	if cfg.AccessKeyID != "" {
		// Static credentials
		awsCfg := aws.Config{
			Region: cfg.Region,
			Credentials: credentials.NewStaticCredentialsProvider(
				cfg.AccessKeyID,
				cfg.SecretAccessKey,
				"",
			),
		}
		client = ec2.NewFromConfig(awsCfg)
	} else {
		// IAM credential chain
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion(cfg.Region),
		)
		if err != nil {
			return nil, fmt.Errorf("ec2: failed to load AWS config: %w", err)
		}
		client = ec2.NewFromConfig(awsCfg)
	}

	return &EC2Pool{
		client: client,
		cfg:    cfg,
	}, nil
}

func (p *EC2Pool) CreateMachine(ctx context.Context, opts MachineOpts) (*Machine, error) {
	instanceType := p.cfg.InstanceType
	if opts.Size != "" {
		instanceType = opts.Size
	}

	userData := p.buildUserData(opts)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String(p.cfg.AMI),
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
