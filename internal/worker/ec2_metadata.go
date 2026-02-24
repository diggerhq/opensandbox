package worker

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// GetEC2InstanceID retrieves the EC2 instance ID from IMDSv2.
// Returns empty string if not running on EC2 or IMDS is unavailable.
func GetEC2InstanceID() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Get IMDSv2 token
	tokenReq, _ := http.NewRequestWithContext(ctx, "PUT", "http://169.254.169.254/latest/api/token", nil)
	tokenReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	tokenResp, err := http.DefaultClient.Do(tokenReq)
	if err != nil {
		return ""
	}
	defer tokenResp.Body.Close()
	tokenBytes, _ := io.ReadAll(tokenResp.Body)
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return ""
	}

	// Get instance ID
	idReq, _ := http.NewRequestWithContext(ctx, "GET", "http://169.254.169.254/latest/meta-data/instance-id", nil)
	idReq.Header.Set("X-aws-ec2-metadata-token", token)
	idResp, err := http.DefaultClient.Do(idReq)
	if err != nil {
		return ""
	}
	defer idResp.Body.Close()
	idBytes, _ := io.ReadAll(idResp.Body)
	return strings.TrimSpace(string(idBytes))
}
