package s3

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigDefaultAndExplicitPath(t *testing.T) {
	t.Chdir(t.TempDir())
	// Old environment settings must not override the JSON configuration.
	t.Setenv("S3_CONFIG_FILE", "missing.json")
	t.Setenv("S3_BUCKET", "env-bucket")
	t.Setenv("S3_ACCESS_KEY_ID", "env-id")
	t.Setenv("S3_SECRET_ACCESS_KEY", "env-secret")
	t.Setenv("S3_TEST_TOKEN", "env-token")
	if err := os.WriteFile("config.json", []byte(`{"storage_path":"./custom","s3":{"bucket":"configured-bucket","part_size":16777216},"key_data":{"keyID":"json-id","applicationKey":"json-secret"},"test_token":"json-token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if c.StoragePath != "./custom" || c.S3.Bucket != "configured-bucket" || c.S3.PartSize != 16777216 || c.S3.AccessKeyID != "json-id" || c.S3.SecretAccessKey != "json-secret" || c.TestToken != "json-token" {
		t.Fatal("JSON configuration not loaded")
	}
	other := filepath.Join(t.TempDir(), "other.json")
	if err := os.WriteFile(other, []byte(`{"s3":{"bucket":"other-bucket"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = LoadConfig(other)
	if err != nil || c.S3.Bucket != "other-bucket" {
		t.Fatalf("explicit config path failed: %v", err)
	}
	if c.S3.AccessKeyID != "" || c.S3.SecretAccessKey != "" {
		t.Fatal("credentials taken from environment")
	}
}

func TestLoadConfigMissingAndInvalid(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("missing default config accepted")
	}
	if err := os.WriteFile("config.json", []byte(`{"s3":{"part_size":"8 * 1024 * 1024"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("string part size accepted")
	}
}
