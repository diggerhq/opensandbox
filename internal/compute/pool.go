package compute

import "context"

// Machine represents a worker machine in the compute pool.
type Machine struct {
	ID       string `json:"id"`
	Addr     string `json:"addr"`     // internal address (host:port) for gRPC
	HTTPAddr string `json:"httpAddr"` // public HTTP address for direct SDK access
	Region   string `json:"region"`
	Status   string `json:"status"`   // "running", "stopped", "creating"
	Capacity int    `json:"capacity"` // max sandboxes
	Current  int    `json:"current"`  // current sandbox count
}

// MachineOpts are options for creating a new machine.
type MachineOpts struct {
	Region string
	Size   string // provider-specific machine size (e.g. "Standard_D16s_v5", "c7gd.metal")
	Image  string // provider-specific image reference (URN, AMI ID, etc.)
}

// WorkerSpec carries cloud-neutral configuration that the control plane
// supplies to a Pool when launching a worker. The Pool combines this with
// its own cloud-specific defaults (mount paths, image layout, NVMe handling)
// to build the worker's cloud-init / user-data.
//
// This decouples the CP from cloud-specific worker bring-up. Adding a new
// cloud means writing a new Pool; the CP doesn't change.
type WorkerSpec struct {
	// Cell + region identity
	CellID string // "{cloud}-{region}-cell-{slot}", e.g. "azure-westus2-cell-b"
	Region string // e.g. "westus2", "us-east-1"

	// Connectivity back to the control plane
	DatabaseURL string
	RedisURL    string

	// Auth
	JWTSecret        string // sandbox-scoped JWT signing key
	SessionJWTSecret string // session JWT shared with api-edge (cf-cutover)

	// CF-cutover event pipe
	CFEventEndpoint string
	CFEventSecret   string
	CFAdminSecret   string

	// Worker capacity + sandbox defaults
	MaxCapacity     int
	SandboxDomain   string
	DefaultMemoryMB int
	DefaultCPUs     int
	DefaultDiskMB   int

	// Object storage (S3-compat: Azure Blob, AWS S3, GCS, R2, MinIO)
	S3Endpoint        string
	S3Bucket          string
	S3Region          string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3ForcePathStyle  bool

	// Optional analytics
	SegmentWriteKey string

	// Provider-specific secrets reference (AWS Secrets Manager ARN, GCP Secret
	// Manager name, etc.). The pool decides what to do with this — Azure pool
	// ignores it (uses Key Vault directly), AWS pool exports it as
	// OPENSANDBOX_SECRETS_ARN.
	SecretsRef string
}

// Pool is the interface for compute pool providers.
//
// Implementations: AzurePool, EC2Pool, LocalPool. Adding a new cloud is
// a new file implementing this interface — no other code changes.
type Pool interface {
	CreateMachine(ctx context.Context, opts MachineOpts) (*Machine, error)
	DestroyMachine(ctx context.Context, machineID string) error
	StartMachine(ctx context.Context, machineID string) error
	StopMachine(ctx context.Context, machineID string) error
	ListMachines(ctx context.Context) ([]*Machine, error)
	HealthCheck(ctx context.Context, machineID string) error
	SupportedRegions(ctx context.Context) ([]string, error)
	DrainMachine(ctx context.Context, machineID string) error
}

// WorkerSpecHolder is implemented by pools that accept a WorkerSpec to
// generate worker cloud-init / user-data. Set once at startup; CreateMachine
// uses it for every subsequent launch.
//
// This is a separate interface (not a Pool method) so LocalPool (testing)
// can ignore it cleanly.
type WorkerSpecHolder interface {
	SetWorkerSpec(spec WorkerSpec)
}

// BuildWorkerEnv produces the contents of /etc/opensandbox/worker.env from a
// WorkerSpec. Pool implementations use this as a starting point and
// concatenate provider-specific lines (paths, machine-id placeholders, etc.).
//
// Lines marked with PLACEHOLDER are patched on the worker by cloud-init
// using the VM's actual private IP and hostname.
func BuildWorkerEnv(spec WorkerSpec) string {
	return buildEnvLines([]envLine{
		{"HOME", "/root"},
		{"OPENSANDBOX_MODE", "worker"},
		{"OPENSANDBOX_VM_BACKEND", "qemu"},
		{"OPENSANDBOX_QEMU_BIN", "qemu-system-x86_64"},
		{"OPENSANDBOX_DATA_DIR", "/data/sandboxes"},
		{"OPENSANDBOX_KERNEL_PATH", "/opt/opensandbox/vmlinux"},
		{"OPENSANDBOX_IMAGES_DIR", "/data/firecracker/images"},
		{"OPENSANDBOX_GRPC_ADVERTISE", "PLACEHOLDER:9090"},
		{"OPENSANDBOX_HTTP_ADDR", "http://PLACEHOLDER:8081"},
		{"OPENSANDBOX_WORKER_ID", "PLACEHOLDER"},
		{"OPENSANDBOX_PORT", "8081"},
		{"OPENSANDBOX_JWT_SECRET", spec.JWTSecret},
		{"OPENSANDBOX_REGION", spec.Region},
		{"OPENSANDBOX_CELL_ID", spec.CellID},
		{"OPENSANDBOX_MAX_CAPACITY", itoa(spec.MaxCapacity)},
		{"OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB", itoa(spec.DefaultMemoryMB)},
		{"OPENSANDBOX_DEFAULT_SANDBOX_CPUS", itoa(spec.DefaultCPUs)},
		{"OPENSANDBOX_DEFAULT_SANDBOX_DISK_MB", itoa(spec.DefaultDiskMB)},
		{"OPENSANDBOX_DATABASE_URL", spec.DatabaseURL},
		{"OPENSANDBOX_REDIS_URL", spec.RedisURL},
		{"OPENSANDBOX_S3_BUCKET", spec.S3Bucket},
		{"OPENSANDBOX_S3_REGION", spec.S3Region},
		{"OPENSANDBOX_S3_ENDPOINT", spec.S3Endpoint},
		{"OPENSANDBOX_S3_ACCESS_KEY_ID", spec.S3AccessKeyID},
		{"OPENSANDBOX_S3_SECRET_ACCESS_KEY", spec.S3SecretAccessKey},
		{"OPENSANDBOX_S3_FORCE_PATH_STYLE", boolStr(spec.S3ForcePathStyle)},
		{"OPENSANDBOX_SANDBOX_DOMAIN", spec.SandboxDomain},
		{"SEGMENT_WRITE_KEY", spec.SegmentWriteKey},
		{"OPENSANDBOX_CF_EVENT_ENDPOINT", spec.CFEventEndpoint},
		{"OPENSANDBOX_CF_EVENT_SECRET", spec.CFEventSecret},
		{"OPENSANDBOX_CF_ADMIN_SECRET", spec.CFAdminSecret},
		{"OPENSANDBOX_SESSION_JWT_SECRET", spec.SessionJWTSecret},
		{"OPENSANDBOX_SECRETS_ARN", spec.SecretsRef},
	})
}

type envLine struct{ key, value string }

func buildEnvLines(lines []envLine) string {
	var n int
	for _, l := range lines {
		n += len(l.key) + len(l.value) + 2
	}
	out := make([]byte, 0, n)
	for _, l := range lines {
		if l.value == "" {
			continue // omit empty values rather than write "KEY=" lines
		}
		out = append(out, l.key...)
		out = append(out, '=')
		out = append(out, l.value...)
		out = append(out, '\n')
	}
	return string(out)
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	// inline conversion to avoid pulling strconv just for this
	if n < 0 {
		return "-" + itoa(-n)
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return ""
}
