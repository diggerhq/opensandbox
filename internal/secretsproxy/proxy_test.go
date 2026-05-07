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

// TestCreateSealedEnvs_NamesIndexBuilt asserts the env-var-name → sealed-token
// index is populated whenever a session is registered. UpdateSecretValue uses
// this index; if it isn't built, refresh-by-name silently misses every time.
func TestCreateSealedEnvs_NamesIndexBuilt(t *testing.T) {
	cases := []struct {
		name        string
		secretEnvs  map[string]string
		allowlist   []string
		wantNames   []string // env var names expected in session.Names
		guestIP     string
	}{
		{
			name:       "secrets only",
			secretEnvs: map[string]string{"API_KEY": "v1", "DB_URL": "v2"},
			allowlist:  nil,
			wantNames:  []string{"API_KEY", "DB_URL"},
			guestIP:    "10.0.0.10",
		},
		{
			name:       "secrets + allowlist",
			secretEnvs: map[string]string{"TOKEN": "v"},
			allowlist:  []string{"api.example.com"},
			wantNames:  []string{"TOKEN"},
			guestIP:    "10.0.0.11",
		},
		{
			name:       "allowlist only",
			secretEnvs: nil,
			allowlist:  []string{"api.example.com"},
			wantNames:  []string{}, // no secrets, no name entries — but session still exists
			guestIP:    "10.0.0.12",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newProxyForTest(t)
			p.CreateSealedEnvs("sb-"+tc.name, tc.guestIP, "10.0.0.1",
				nil, tc.secretEnvs, tc.allowlist, nil)
			sess := p.sessions[tc.guestIP]
			if sess == nil {
				t.Fatal("expected session to be registered")
			}
			if len(sess.Names) != len(tc.wantNames) {
				t.Errorf("session.Names len = %d, want %d (names=%v)", len(sess.Names), len(tc.wantNames), sess.Names)
			}
			for _, n := range tc.wantNames {
				token, ok := sess.Names[n]
				if !ok {
					t.Errorf("expected Names[%q] to be set", n)
					continue
				}
				// And the token must point back to a real entry in Secrets.
				if _, ok := sess.Secrets[token]; !ok {
					t.Errorf("Names[%q] = %q but Secrets has no such token (index out of sync)", n, token)
				}
			}
		})
	}
}

// TestUpdateSecretValue_HappyPath replaces the value the sealed token resolves
// to without changing the token id. Sandbox env vars stay the same; the next
// outbound HTTPS substitution uses the new value.
func TestUpdateSecretValue_HappyPath(t *testing.T) {
	p := newProxyForTest(t)
	p.CreateSealedEnvs("sb-update", "10.0.0.20", "10.0.0.1",
		nil,
		map[string]string{"API_KEY": "old-value"},
		nil, nil,
	)
	sess := p.sessions["10.0.0.20"]
	if sess == nil {
		t.Fatal("expected session")
	}
	originalToken, ok := sess.Names["API_KEY"]
	if !ok {
		t.Fatal("expected API_KEY in Names index")
	}

	if !p.UpdateSecretValue("sb-update", "API_KEY", "new-value") {
		t.Fatal("UpdateSecretValue returned false on a known sandbox+name")
	}

	// Sealed token id must NOT change — VM env vars hold this string.
	tokenAfter, ok := sess.Names["API_KEY"]
	if !ok || tokenAfter != originalToken {
		t.Errorf("sealed token id changed: before=%q after=%q (must be stable so guest env vars stay valid)", originalToken, tokenAfter)
	}
	if got := sess.Secrets[originalToken]; got != "new-value" {
		t.Errorf("Secrets[token] = %q, want %q", got, "new-value")
	}
}

// TestUpdateSecretValue_NameMiss covers a refresh for a name the session
// doesn't know about — possible mid-migration or if the customer added a new
// secret to the store after the sandbox was created (env vars are baked at
// create-time, so a brand new name has no sealed token in the session).
// Returns false; caller logs but doesn't treat as fatal.
func TestUpdateSecretValue_NameMiss(t *testing.T) {
	p := newProxyForTest(t)
	p.CreateSealedEnvs("sb-miss", "10.0.0.21", "10.0.0.1",
		nil,
		map[string]string{"API_KEY": "v"},
		nil, nil,
	)
	if p.UpdateSecretValue("sb-miss", "UNKNOWN_NAME", "x") {
		t.Error("UpdateSecretValue returned true for an unknown name; expected false")
	}
	// Existing secret untouched.
	sess := p.sessions["10.0.0.21"]
	tok := sess.Names["API_KEY"]
	if sess.Secrets[tok] != "v" {
		t.Errorf("existing secret value clobbered by miss; got %q", sess.Secrets[tok])
	}
}

// TestUpdateSecretValue_NoSession covers fanout to a worker that has no
// session for the sandbox — happens during the brief window where the source
// has unregistered post-migration but the destination hasn't re-registered
// yet, or when the worker simply isn't hosting the sandbox. Must not panic
// and must return false.
func TestUpdateSecretValue_NoSession(t *testing.T) {
	p := newProxyForTest(t)
	if p.UpdateSecretValue("sb-nope", "API_KEY", "x") {
		t.Error("UpdateSecretValue returned true with no registered session; expected false")
	}
}

// TestUpdateSecretValue_NamesNilNoPanic guards the "session exists but Names
// is nil" case — possible for sessions registered before the SealedNames
// index existed (e.g. a sandbox hibernated on an old binary, woken on a new
// one with no SealedNames in snapshot-meta.json). Must return false, not
// crash.
func TestUpdateSecretValue_NamesNilNoPanic(t *testing.T) {
	p := newProxyForTest(t)
	p.RegisterSession("10.0.0.22", &Session{
		SandboxID: "sb-legacy",
		Secrets:   map[string]string{"osb_sealed_xxx": "value"},
		// Names intentionally nil — pre-fix session shape.
	})
	if p.UpdateSecretValue("sb-legacy", "API_KEY", "x") {
		t.Error("UpdateSecretValue returned true on a session with nil Names; expected false")
	}
}
