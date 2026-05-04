package secretsproxy

import (
	"io"
	"log"
	"reflect"
	"testing"
)

// newProxyForTest builds a SecretsProxy with no listener — sufficient for
// exercising session registration / CreateSealedEnvs without going on the wire.
//
// Sets the package-global log output to io.Discard for the duration of the
// test. bench_test.go leaves log.SetOutput(nil) behind on cleanup, which would
// otherwise panic any log.Printf in tests that run after it.
func newProxyForTest(t *testing.T) *SecretsProxy {
	t.Helper()
	log.SetOutput(io.Discard)
	return &SecretsProxy{
		sessions: make(map[string]*Session),
		closed:   make(chan struct{}),
	}
}

func TestCreateSealedEnvs_AllowlistOnlyRegistersSession(t *testing.T) {
	p := newProxyForTest(t)

	allowlist := []string{"example.com", "api.openai.com"}
	envs := p.CreateSealedEnvs("sb-test", "10.0.0.5", "10.0.0.1",
		map[string]string{"FOO": "bar"}, // user envs
		nil,                             // no secret envs
		allowlist,                       // allowlist only
		nil,
	)

	if envs == nil {
		t.Fatal("CreateSealedEnvs returned nil; expected non-nil env map for allowlist-only sandbox")
	}
	if envs["HTTPS_PROXY"] == "" {
		t.Errorf("expected HTTPS_PROXY to be injected when allowlist is set; got %q", envs["HTTPS_PROXY"])
	}

	got := p.sessions["10.0.0.5"]
	if got == nil {
		t.Fatal("no session registered for guest IP; allowlist-only sandboxes must still get a session")
	}
	if !reflect.DeepEqual(got.Allowlist, allowlist) {
		t.Errorf("session allowlist = %v, want %v", got.Allowlist, allowlist)
	}
	if len(got.Secrets) != 0 {
		t.Errorf("session unexpectedly has secrets: %v", got.Secrets)
	}
}

func TestCreateSealedEnvs_NoEnvsNoSecretsNoAllowlistReturnsNil(t *testing.T) {
	p := newProxyForTest(t)
	envs := p.CreateSealedEnvs("sb-empty", "10.0.0.6", "10.0.0.1", nil, nil, nil, nil)
	if envs != nil {
		t.Errorf("expected nil env map when there is nothing to enforce; got %v", envs)
	}
	if _, exists := p.sessions["10.0.0.6"]; exists {
		t.Errorf("did not expect a registered session when there is nothing to enforce")
	}
}

func TestCreateSealedEnvs_AllowlistOnlyEnforcesViaSession(t *testing.T) {
	p := newProxyForTest(t)
	p.CreateSealedEnvs("sb-allow", "10.0.0.7", "10.0.0.1",
		map[string]string{"K": "v"},
		nil,
		[]string{"example.com"},
		nil,
	)

	sess := p.sessions["10.0.0.7"]
	if sess == nil {
		t.Fatal("allowlist-only session should be registered")
	}
	if !hostAllowed("example.com", sess.Allowlist) {
		t.Errorf("example.com should be allowed by registered session")
	}
	if hostAllowed("evil.com", sess.Allowlist) {
		t.Errorf("evil.com should NOT be allowed by registered session")
	}
}

func TestCreateSealedEnvs_SecretsOnlyStillRegisters(t *testing.T) {
	// Regression guard: the pre-fix behavior (register only when secrets exist)
	// must still hold. Sandboxes with secrets but no allowlist get a session.
	p := newProxyForTest(t)
	p.CreateSealedEnvs("sb-secret", "10.0.0.8", "10.0.0.1",
		nil,
		map[string]string{"API_KEY": "real-secret"},
		nil,
		nil,
	)

	sess := p.sessions["10.0.0.8"]
	if sess == nil {
		t.Fatal("session must still register when only secrets are present")
	}
	if len(sess.Secrets) != 1 {
		t.Errorf("expected 1 sealed secret in session; got %d", len(sess.Secrets))
	}
	if len(sess.Allowlist) != 0 {
		t.Errorf("expected empty allowlist; got %v", sess.Allowlist)
	}
}
