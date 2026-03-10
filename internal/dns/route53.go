package dns

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
)

// Route53Client manages DNS records in a Route53 hosted zone.
type Route53Client struct {
	client       *route53.Client
	hostedZoneID string
}

// Route53Config holds the configuration for a Route53 client.
type Route53Config struct {
	HostedZoneID   string
	Region         string
	AccessKeyID    string
	SecretAccessKey string
}

// NewRoute53Client creates a new Route53 client.
// If AccessKeyID is empty, uses the default AWS credential chain (IAM instance profile).
func NewRoute53Client(cfg Route53Config) (*Route53Client, error) {
	var opts []func(*awsconfig.LoadOptions) error

	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	if cfg.AccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	return &Route53Client{
		client:       route53.NewFromConfig(awsCfg),
		hostedZoneID: cfg.HostedZoneID,
	}, nil
}

// UpsertARecord creates or updates an A record.
func (c *Route53Client) UpsertARecord(ctx context.Context, hostname, ip string, ttl int) error {
	if ttl <= 0 {
		ttl = 60
	}
	ttl64 := int64(ttl)

	input := &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: &c.hostedZoneID,
		ChangeBatch: &r53types.ChangeBatch{
			Changes: []r53types.Change{
				{
					Action: r53types.ChangeActionUpsert,
					ResourceRecordSet: &r53types.ResourceRecordSet{
						Name: &hostname,
						Type: r53types.RRTypeA,
						TTL:  &ttl64,
						ResourceRecords: []r53types.ResourceRecord{
							{Value: &ip},
						},
					},
				},
			},
		},
	}

	resp, err := c.client.ChangeResourceRecordSets(ctx, input)
	if err != nil {
		return fmt.Errorf("upsert A record %s -> %s: %w", hostname, ip, err)
	}

	log.Printf("dns: upserted A record %s -> %s (change=%s)", hostname, ip, *resp.ChangeInfo.Id)
	return nil
}

// DeleteARecord removes an A record. The ttl must match the existing record's TTL
// for Route53 to accept the deletion.
func (c *Route53Client) DeleteARecord(ctx context.Context, hostname, ip string, ttl int64) error {
	if ttl <= 0 {
		ttl = 60
	}
	ttl64 := ttl

	input := &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: &c.hostedZoneID,
		ChangeBatch: &r53types.ChangeBatch{
			Changes: []r53types.Change{
				{
					Action: r53types.ChangeActionDelete,
					ResourceRecordSet: &r53types.ResourceRecordSet{
						Name: &hostname,
						Type: r53types.RRTypeA,
						TTL:  &ttl64,
						ResourceRecords: []r53types.ResourceRecord{
							{Value: &ip},
						},
					},
				},
			},
		},
	}

	_, err := c.client.ChangeResourceRecordSets(ctx, input)
	if err != nil {
		return fmt.Errorf("delete A record %s: %w", hostname, err)
	}

	log.Printf("dns: deleted A record %s", hostname)
	return nil
}

// UpsertTXTRecord creates or updates a TXT record (used for ACME DNS-01 challenges).
func (c *Route53Client) UpsertTXTRecord(ctx context.Context, name, value string, ttl int) error {
	if ttl <= 0 {
		ttl = 60
	}
	ttl64 := int64(ttl)
	// TXT records must be quoted
	quotedValue := fmt.Sprintf(`"%s"`, value)

	input := &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: &c.hostedZoneID,
		ChangeBatch: &r53types.ChangeBatch{
			Changes: []r53types.Change{
				{
					Action: r53types.ChangeActionUpsert,
					ResourceRecordSet: &r53types.ResourceRecordSet{
						Name: &name,
						Type: r53types.RRTypeTxt,
						TTL:  &ttl64,
						ResourceRecords: []r53types.ResourceRecord{
							{Value: &quotedValue},
						},
					},
				},
			},
		},
	}

	_, err := c.client.ChangeResourceRecordSets(ctx, input)
	if err != nil {
		return fmt.Errorf("upsert TXT record %s: %w", name, err)
	}

	log.Printf("dns: upserted TXT record %s", name)
	return nil
}

// DeleteTXTRecord removes a TXT record.
func (c *Route53Client) DeleteTXTRecord(ctx context.Context, name, value string) error {
	ttl64 := int64(60)
	quotedValue := fmt.Sprintf(`"%s"`, value)

	input := &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: &c.hostedZoneID,
		ChangeBatch: &r53types.ChangeBatch{
			Changes: []r53types.Change{
				{
					Action: r53types.ChangeActionDelete,
					ResourceRecordSet: &r53types.ResourceRecordSet{
						Name: &name,
						Type: r53types.RRTypeTxt,
						TTL:  &ttl64,
						ResourceRecords: []r53types.ResourceRecord{
							{Value: &quotedValue},
						},
					},
				},
			},
		},
	}

	_, err := c.client.ChangeResourceRecordSets(ctx, input)
	if err != nil {
		return fmt.Errorf("delete TXT record %s: %w", name, err)
	}

	log.Printf("dns: deleted TXT record %s", name)
	return nil
}

// ARecord represents a DNS A record.
type ARecord struct {
	Name  string
	Value string // IP address
	TTL   int64
}

// ListARecords returns all A records in the hosted zone that match the given suffix.
// For example, suffix ".workers.opensandbox.ai" returns all worker A records.
func (c *Route53Client) ListARecords(ctx context.Context, suffix string) ([]ARecord, error) {
	var records []ARecord
	var nextName *string
	var nextType r53types.RRType

	for {
		input := &route53.ListResourceRecordSetsInput{
			HostedZoneId: &c.hostedZoneID,
		}
		if nextName != nil {
			input.StartRecordName = nextName
			input.StartRecordType = nextType
		}

		resp, err := c.client.ListResourceRecordSets(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("list records: %w", err)
		}

		for _, rrs := range resp.ResourceRecordSets {
			if rrs.Type != r53types.RRTypeA {
				continue
			}
			name := *rrs.Name
			// Route53 returns names with trailing dot
			if len(name) > 0 && name[len(name)-1] == '.' {
				name = name[:len(name)-1]
			}
			if suffix != "" && !strings.HasSuffix(name, suffix) {
				continue
			}
			for _, rr := range rrs.ResourceRecords {
				records = append(records, ARecord{
					Name:  name,
					Value: *rr.Value,
					TTL:   *rrs.TTL,
				})
			}
		}

		if !resp.IsTruncated {
			break
		}
		nextName = resp.NextRecordName
		nextType = resp.NextRecordType
	}

	return records, nil
}

// WaitForChange waits for a Route53 change to propagate (INSYNC status).
func (c *Route53Client) WaitForChange(ctx context.Context, changeID string) error {
	for {
		resp, err := c.client.GetChange(ctx, &route53.GetChangeInput{
			Id: &changeID,
		})
		if err != nil {
			return fmt.Errorf("get change status: %w", err)
		}
		if resp.ChangeInfo.Status == r53types.ChangeStatusInsync {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
