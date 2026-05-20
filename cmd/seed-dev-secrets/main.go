// seed-dev-secrets inserts known-plaintext secrets into the dev cell PG,
// encrypted the SAME way the legacy CP encrypts them (via the keyring, which
// prepends a 2-byte version header). This gives the PG→D1 backfill real
// keyring-format ciphertext to decrypt-and-re-encrypt, and gives us known
// plaintexts to assert against after backfill.
//
// Run ON the dev CP VM (it has both PG reachability and the shared key):
//
//	OPENSANDBOX_DATABASE_URL=... OPENSANDBOX_SECRET_ENCRYPTION_KEY=... \
//	  ./seed-dev-secrets [--dry-run]
//
// --verify mode: after the backfill has run, fetch the 10 entries back from
// D1 and assert each decrypts to its known plaintext via BOTH consumers:
//   - CP path:   keyring.Decrypt (the real runtime decryptor, edgeclient.go)
//   - edge path: NewEncryptor.Decrypt — byte-identical to api-edge/src/crypto.ts
//                decryptSecret (nonce = blob[0:12], AES-GCM)
//
//	OPENSANDBOX_SECRET_ENCRYPTION_KEY=... OPENSANDBOX_CF_API_TOKEN=... \
//	  ./seed-dev-secrets --verify --account=<acct> --database=<d1-id>
//
// Idempotent (fixed UUIDs + ON CONFLICT DO NOTHING).
//
// Teardown:
//   DELETE FROM secret_store_entries WHERE id::text LIKE 'e7000000-%';
//   DELETE FROM secret_stores        WHERE id::text LIKE 'c5000000-%';
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/opensandbox/opensandbox/internal/crypto"
)

type secret struct {
	storeIdx     int
	entryID      string
	name         string
	plaintext    string
	allowedHosts []string
}

var stores = []struct {
	id, orgID, name string
	egress          []string
}{
	{"c5000000-0000-0000-0000-000000000001", "aaaaaaaa-0000-0000-0000-000000000001", "ks-store-a", []string{"api.openai.com"}},
	{"c5000000-0000-0000-0000-000000000002", "aaaaaaaa-0000-0000-0000-000000000002", "ks-store-b", []string{"*.github.com", "api.stripe.com"}},
	{"c5000000-0000-0000-0000-000000000003", "aaaaaaaa-0000-0000-0000-000000000003", "ks-store-c", []string{}},
}

var secrets = []secret{
	{1, "e7000000-0000-0000-0000-000000000001", "OPENAI_API_KEY", "sk-test-openai-abc123", []string{"api.openai.com"}},
	{1, "e7000000-0000-0000-0000-000000000002", "GITHUB_TOKEN", "ghp_testtoken456", []string{"*.github.com"}},
	{1, "e7000000-0000-0000-0000-000000000003", "DB_PASSWORD", "p@ssw0rd-with-symbols!#$", []string{}},
	{1, "e7000000-0000-0000-0000-000000000004", "EMPTYISH", "", []string{}},
	{2, "e7000000-0000-0000-0000-000000000005", "STRIPE_SECRET_KEY", "sk_test_stripe_789", []string{"api.stripe.com"}},
	{2, "e7000000-0000-0000-0000-000000000006", "SLACK_BOT_TOKEN", "xoxb-slack-bot-0001", []string{"slack.com"}},
	{2, "e7000000-0000-0000-0000-000000000007", "UNICODE_VALUE", "héllo-wörld-🔐-secret", []string{}},
	{3, "e7000000-0000-0000-0000-000000000008", "AWS_SECRET_ACCESS_KEY", "aws/secret+with/slashes", []string{"*.amazonaws.com"}},
	{3, "e7000000-0000-0000-0000-000000000009", "WEBHOOK_SECRET", "whsec_longvalue_0123456789abcdef", []string{}},
	{3, "e7000000-0000-0000-0000-000000000010", "JWT_SIGNING_KEY", "a-256-bit-signing-key-value-here", []string{}},
}

func main() {
	dryRun := flag.Bool("dry-run", false, "encrypt + print the plaintext map but don't write to PG")
	verify := flag.Bool("verify", false, "fetch the backfilled entries from D1 and assert they decrypt to the known plaintexts")
	account := flag.String("account", "", "CF account id (verify mode)")
	database := flag.String("database", "", "D1 database id (verify mode)")
	flag.Parse()

	if os.Getenv("OPENSANDBOX_SECRET_ENCRYPTION_KEY") == "" {
		log.Fatal("OPENSANDBOX_SECRET_ENCRYPTION_KEY is required")
	}
	ring, err := crypto.NewKeyRingFromEnv()
	if err != nil {
		log.Fatalf("build keyring: %v", err)
	}
	if ring == nil {
		log.Fatal("keyring is nil — OPENSANDBOX_SECRET_ENCRYPTION_KEY not set")
	}

	if *verify {
		runVerify(ring, *account, *database)
		return
	}

	runSeed(ring, *dryRun)
}

func runSeed(ring *crypto.KeyRing, dryRun bool) {
	dbURL := os.Getenv("OPENSANDBOX_DATABASE_URL")
	if dbURL == "" {
		log.Fatal("OPENSANDBOX_DATABASE_URL is required")
	}
	enc := ring.AsEncryptor() // keyring-backed → version-prefixed, like legacy CP secrets

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect pg: %v", err)
	}
	defer conn.Close(ctx)

	for _, s := range stores {
		if dryRun {
			log.Printf("[dry-run] store %s (%s) org=%s", s.id, s.name, s.orgID)
			continue
		}
		if _, err := conn.Exec(ctx,
			`INSERT INTO secret_stores (id, org_id, name, egress_allowlist)
			 VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING`,
			s.id, s.orgID, s.name, s.egress); err != nil {
			log.Fatalf("insert store %s: %v", s.name, err)
		}
	}

	log.Printf("=== known secrets (entry_id | store.name | plaintext) ===")
	for _, sec := range secrets {
		st := stores[sec.storeIdx-1]
		ct, err := enc.Encrypt([]byte(sec.plaintext))
		if err != nil {
			log.Fatalf("encrypt %s: %v", sec.name, err)
		}
		log.Printf("%s | %s.%s | %q", sec.entryID, st.name, sec.name, sec.plaintext)
		if dryRun {
			continue
		}
		if _, err := conn.Exec(ctx,
			`INSERT INTO secret_store_entries (id, store_id, name, encrypted_value, allowed_hosts)
			 VALUES ($1, $2, $3, $4, $5) ON CONFLICT (id) DO NOTHING`,
			sec.entryID, st.id, sec.name, ct, sec.allowedHosts); err != nil {
			log.Fatalf("insert entry %s: %v", sec.name, err)
		}
	}
	if dryRun {
		log.Printf("DONE (dry-run, nothing written)")
	} else {
		log.Printf("DONE: seeded %d stores + %d known secrets", len(stores), len(secrets))
	}
}

func runVerify(ring *crypto.KeyRing, account, database string) {
	token := os.Getenv("OPENSANDBOX_CF_API_TOKEN")
	if account == "" || database == "" || token == "" {
		log.Fatal("--account, --database, and OPENSANDBOX_CF_API_TOKEN are required for --verify")
	}
	// bareEnc.Decrypt replicates the edge's decryptSecret exactly (nonce=blob[0:12]).
	bareEnc, err := crypto.NewEncryptor(os.Getenv("OPENSANDBOX_SECRET_ENCRYPTION_KEY"))
	if err != nil {
		log.Fatalf("build bare encryptor: %v", err)
	}

	ids := make([]string, len(secrets))
	for i, s := range secrets {
		ids[i] = "'" + s.entryID + "'"
	}
	sql := fmt.Sprintf("SELECT id, hex(encrypted_value) AS hexv FROM secret_store_entries WHERE id IN (%s)", strings.Join(ids, ","))

	rows, err := d1Query(account, database, token, sql)
	if err != nil {
		log.Fatalf("d1 query: %v", err)
	}
	got := map[string]string{} // entry_id → hex ciphertext
	for _, r := range rows {
		id, _ := r["id"].(string)
		hv, _ := r["hexv"].(string)
		got[id] = hv
	}

	pass, fail := 0, 0
	for _, sec := range secrets {
		hv, ok := got[sec.entryID]
		if !ok {
			log.Printf("FAIL %s (%s): not found in D1", sec.name, sec.entryID)
			fail++
			continue
		}
		blob, err := hex.DecodeString(hv)
		if err != nil {
			log.Printf("FAIL %s: bad hex from D1: %v", sec.name, err)
			fail++
			continue
		}
		cpPT, cpErr := ring.Decrypt(blob)             // CP runtime path
		edgePT, edgeErr := bareEnc.Decrypt(blob)      // edge decryptSecret replica
		cpOK := cpErr == nil && string(cpPT) == sec.plaintext
		edgeOK := edgeErr == nil && string(edgePT) == sec.plaintext
		if cpOK && edgeOK {
			log.Printf("PASS %s: CP+edge both decrypt to %q", sec.name, sec.plaintext)
			pass++
		} else {
			log.Printf("FAIL %s: cpOK=%v (err=%v) edgeOK=%v (err=%v)", sec.name, cpOK, cpErr, edgeOK, edgeErr)
			fail++
		}
	}
	log.Printf("=== VERIFY: %d pass, %d fail ===", pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
}

func d1Query(account, database, token, sql string) ([]map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"sql": sql})
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/d1/database/%s/query", account, database)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var parsed struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Result []struct {
			Results []map[string]any `json:"results"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode (http %d): %w", resp.StatusCode, err)
	}
	if !parsed.Success {
		if len(parsed.Errors) > 0 {
			return nil, fmt.Errorf("d1: %s", parsed.Errors[0].Message)
		}
		return nil, fmt.Errorf("d1 failed (http %d)", resp.StatusCode)
	}
	if len(parsed.Result) == 0 {
		return nil, nil
	}
	return parsed.Result[0].Results, nil
}
