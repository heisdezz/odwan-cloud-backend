// Package s3 uploads local files to S3-compatible storage with durable resume state.
package s3

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	StoragePath    string         `json:"storage_path"`
	S3             S3Config       `json:"s3"`
	KeyData        KeyData        `json:"key_data"`
	TestToken      string         `json:"test_token"`
	Telegram       TelegramConfig `json:"telegram"`
	StorageBackend string         `json:"storage_backend"`
}

// TelegramConfig holds the Bot API credentials used by TgNAS.
type TelegramConfig struct {
	BotToken    string `json:"bot_token"`
	ChatID      string `json:"chat_id"`
	Bucket      string `json:"bucket"`
	ChunkSize   int64  `json:"chunk_size"`
	MaxFileSize int64  `json:"max_file_size"`
	APIBaseURL  string `json:"api_base_url"`
}

type KeyData struct {
	KeyID          string `json:"keyID"`
	KeyName        string `json:"keyName"`
	ApplicationKey string `json:"applicationKey"`
	Location       string `json:"location"`
	ID             string `json:"id"`
	BucketName     string `json:"bucketName"`
}

type S3Config struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	PartSize        int64  `json:"part_size"`
	Concurrency     int    `json:"concurrency"`
	AccessKeyID     string `json:"-"`
	SecretAccessKey string `json:"-"`
}

// LoadConfig reads all settings, including credentials, from config.json by default.
func LoadConfig(path string) (Config, error) {
	c := Config{StoragePath: "./storage", StorageBackend: "s3", S3: S3Config{Endpoint: "https://s3.eu-central-003.backblazeb2.com", Region: "eu-central-003", Bucket: "odw-cloud", PartSize: 8 << 20, Concurrency: 4}}
	if path == "" {
		path = "config.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("decode config %s: %w", path, err)
	}
	c.S3.AccessKeyID = c.KeyData.KeyID
	c.S3.SecretAccessKey = c.KeyData.ApplicationKey
	return c, nil
}

func (c Config) validate() error {
	u, err := url.Parse(c.S3.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid S3 endpoint")
	}
	if strings.TrimSpace(c.StoragePath) == "" || c.S3.Region == "" || c.S3.Bucket == "" {
		return fmt.Errorf("storage_path, S3 region and bucket are required")
	}
	if c.S3.AccessKeyID == "" || c.S3.SecretAccessKey == "" {
		return fmt.Errorf("set key_data.keyID and key_data.applicationKey in config.json")
	}
	if c.S3.PartSize < 5<<20 || c.S3.PartSize > 5<<30 {
		return fmt.Errorf("part_size must be between 5 MiB and 5 GiB")
	}
	if c.S3.Concurrency < 1 || c.S3.Concurrency > 32 {
		return fmt.Errorf("concurrency must be between 1 and 32")
	}
	return nil
}
