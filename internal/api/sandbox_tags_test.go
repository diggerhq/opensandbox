package api

import (
	"strings"
	"testing"
)

// Validation tests — pure handler logic, no DB.

func TestValidateTags_OK(t *testing.T) {
	err := validateTags(map[string]string{
		"env":           "prod",
		"team":          "payments",
		"team:payments": "yes", // `:` allowed in keys per design
		"empty":         "",     // empty values allowed
	})
	if err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestValidateTags_TooMany(t *testing.T) {
	tags := make(map[string]string, maxTagsPerSandbox+1)
	for i := 0; i <= maxTagsPerSandbox; i++ {
		tags[strings.Repeat("a", 1)+strings.Repeat("b", i%4+1)+indexStr(i)] = "v"
	}
	err := validateTags(tags)
	if err == nil || !strings.Contains(err.Error(), "max 50 tags") {
		t.Errorf("expected max-tags error, got %v", err)
	}
}

func TestValidateTags_KeyLength(t *testing.T) {
	cases := []struct {
		key     string
		wantErr bool
	}{
		{"", true},
		{strings.Repeat("a", maxTagKeyLen), false},
		{strings.Repeat("a", maxTagKeyLen+1), true},
	}
	for _, tc := range cases {
		err := validateTags(map[string]string{tc.key: "v"})
		if (err != nil) != tc.wantErr {
			t.Errorf("key len=%d: err=%v wantErr=%v", len(tc.key), err, tc.wantErr)
		}
	}
}

func TestValidateTags_KeyCharset(t *testing.T) {
	bad := []string{"has space", "has/slash", "emoji🙂", "quote\"key"}
	for _, k := range bad {
		if err := validateTags(map[string]string{k: "v"}); err == nil {
			t.Errorf("expected invalid for key %q", k)
		}
	}
}

func TestValidateTags_ReservedPrefix(t *testing.T) {
	err := validateTags(map[string]string{"oc:system": "no"})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("expected reserved-prefix error, got %v", err)
	}
}

func TestValidateTags_ValueLength(t *testing.T) {
	err := validateTags(map[string]string{
		"k": strings.Repeat("v", maxTagValueLen+1),
	})
	if err == nil || !strings.Contains(err.Error(), "length") {
		t.Errorf("expected value-length error, got %v", err)
	}
	err = validateTags(map[string]string{
		"k": strings.Repeat("v", maxTagValueLen),
	})
	if err != nil {
		t.Errorf("max-length value should pass, got %v", err)
	}
}

// indexStr avoids importing strconv for a one-liner in the too-many test.
func indexStr(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
