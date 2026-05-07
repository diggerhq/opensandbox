package config

import (
	"os"
	"reflect"
	"testing"
)

func TestSplitCSV(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   ,  ,", nil},
		{"single", "Standard_D16ads_v7", []string{"Standard_D16ads_v7"}},
		{"multiple", "Standard_D16ads_v7,Standard_D16ds_v6,Standard_D16s_v5",
			[]string{"Standard_D16ads_v7", "Standard_D16ds_v6", "Standard_D16s_v5"}},
		{"trim whitespace", "  Standard_D16ads_v7 , Standard_D16ds_v6  ",
			[]string{"Standard_D16ads_v7", "Standard_D16ds_v6"}},
		{"drop empty entries", "a,,b,,,c", []string{"a", "b", "c"}},
		{"order preserved", "c7gd.metal,r7gd.metal,r6gd.metal",
			[]string{"c7gd.metal", "r7gd.metal", "r6gd.metal"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitCSV(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("splitCSV(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestLoadParsesMachineSizeLists(t *testing.T) {
	t.Setenv("OPENSANDBOX_AZURE_VM_SIZES", "Standard_D16ads_v7, Standard_D16ds_v6 ,Standard_D16s_v5")
	t.Setenv("OPENSANDBOX_EC2_INSTANCE_TYPES", "c7gd.metal,r7gd.metal")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	wantAzure := []string{"Standard_D16ads_v7", "Standard_D16ds_v6", "Standard_D16s_v5"}
	if !reflect.DeepEqual(cfg.AzureVMSizes, wantAzure) {
		t.Errorf("AzureVMSizes = %v, want %v", cfg.AzureVMSizes, wantAzure)
	}
	wantEC2 := []string{"c7gd.metal", "r7gd.metal"}
	if !reflect.DeepEqual(cfg.EC2InstanceTypes, wantEC2) {
		t.Errorf("EC2InstanceTypes = %v, want %v", cfg.EC2InstanceTypes, wantEC2)
	}
}

func TestLoadDefaults(t *testing.T) {
	// Clear env to test defaults
	os.Unsetenv("OPENSANDBOX_PORT")
	os.Unsetenv("OPENSANDBOX_API_KEY")
	os.Unsetenv("OPENSANDBOX_WORKER_ADDR")
	os.Unsetenv("OPENSANDBOX_MODE")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.Port)
	}
	if cfg.Mode != "combined" {
		t.Errorf("expected mode combined, got %s", cfg.Mode)
	}
	if cfg.WorkerAddr != "localhost:9090" {
		t.Errorf("expected worker addr localhost:9090, got %s", cfg.WorkerAddr)
	}
}

func TestLoadFromEnv(t *testing.T) {
	os.Setenv("OPENSANDBOX_PORT", "9999")
	os.Setenv("OPENSANDBOX_API_KEY", "test-key")
	os.Setenv("OPENSANDBOX_MODE", "server")
	defer func() {
		os.Unsetenv("OPENSANDBOX_PORT")
		os.Unsetenv("OPENSANDBOX_API_KEY")
		os.Unsetenv("OPENSANDBOX_MODE")
	}()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Port != 9999 {
		t.Errorf("expected port 9999, got %d", cfg.Port)
	}
	if cfg.APIKey != "test-key" {
		t.Errorf("expected API key test-key, got %s", cfg.APIKey)
	}
	if cfg.Mode != "server" {
		t.Errorf("expected mode server, got %s", cfg.Mode)
	}
}

func TestLoadInvalidPort(t *testing.T) {
	os.Setenv("OPENSANDBOX_PORT", "not-a-number")
	defer os.Unsetenv("OPENSANDBOX_PORT")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid port, got nil")
	}
}
