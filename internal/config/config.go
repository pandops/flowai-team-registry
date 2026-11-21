// Package config loads the team-registry YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultSchema is the default PostgreSQL schema of team-registry.
const DefaultSchema = "team_registry_schema"

// Config is the complete runtime configuration.
type Config struct {
	HTTP   HTTP   `yaml:"http"`
	DB     DB     `yaml:"db"`
	NATS   NATS   `yaml:"nats"`
	Outbox Outbox `yaml:"outbox"`
}

// HTTP configures the probe listener.
type HTTP struct {
	Listen string `yaml:"listen"`
}

// DB configures the PostgreSQL connection.
type DB struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Name     string `yaml:"name"`
	Schema   string `yaml:"schema"`
	SSLMode  string `yaml:"sslmode"`
	CAFile   string `yaml:"ca_file"`
}

// NATS configures the broker connection.
type NATS struct {
	URL             string        `yaml:"url"`
	CredentialsFile string        `yaml:"credentials_file"`
	CAFile          string        `yaml:"ca_file"`
	InboxPrefix     string        `yaml:"inbox_prefix"`
	RequestTimeout  time.Duration `yaml:"request_timeout"`
}

// Outbox configures the relay.
type Outbox struct {
	PollInterval time.Duration `yaml:"poll_interval"`
}

var schemaIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// dbHost is one DNS name: no scheme, credentials, port, path, query, list
// or keyword option can ride in db.host into the connection URL.
var dbHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// Parse decodes and validates a YAML document. Unknown keys, placeholder
// values and any setting below the production trust rules are rejected.
func Parse(data []byte) (Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, errors.New("config: empty document")
		}
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if cfg.HTTP.Listen == "" {
		cfg.HTTP.Listen = ":8080"
	}
	if cfg.DB.Port == 0 {
		cfg.DB.Port = 5432
	}
	if cfg.DB.Name == "" {
		cfg.DB.Name = "flowai"
	}
	if cfg.DB.Schema == "" {
		cfg.DB.Schema = DefaultSchema
	}
	if cfg.NATS.InboxPrefix == "" {
		cfg.NATS.InboxPrefix = "_INBOX.team-registry"
	}
	if cfg.NATS.RequestTimeout == 0 {
		cfg.NATS.RequestTimeout = 5 * time.Second
	}
	if cfg.Outbox.PollInterval == 0 {
		cfg.Outbox.PollInterval = 500 * time.Millisecond
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

func (c Config) validate() error {
	if p := placeholder(reflect.ValueOf(c), ""); p != "" {
		return fmt.Errorf("%s still holds a <...> placeholder", p)
	}
	var problems []string
	need := func(ok bool, msg string) {
		if !ok {
			problems = append(problems, msg)
		}
	}
	need(c.DB.Host != "", "db.host is required")
	need(c.DB.Host == "" || validHost(c.DB.Host), "db.host must be a plain host name or IPv4 address")
	need(c.DB.Port > 0 && c.DB.Port < 65536, "db.port is out of range")
	need(c.DB.User != "", "db.user is required")
	need(c.DB.Password != "", "db.password is required")
	need(schemaIdent.MatchString(c.DB.Name), "db.name is not a plain identifier")
	need(schemaIdent.MatchString(c.DB.Schema), "db.schema is not a lowercase identifier of at most 63 characters")
	need(c.DB.SSLMode == "verify-full", "db.sslmode must be verify-full")
	need(fileExists(c.DB.CAFile), "db.ca_file must name an existing file")
	need(tlsURLs(c.NATS.URL), "nats.url must be one or more tls:// URLs")
	need(fileExists(c.NATS.CredentialsFile), "nats.credentials_file must name an existing file")
	need(fileExists(c.NATS.CAFile), "nats.ca_file must name an existing file")
	need(strings.HasPrefix(c.NATS.InboxPrefix, "_INBOX."), "nats.inbox_prefix must start with _INBOX.")
	need(c.NATS.RequestTimeout > 0, "nats.request_timeout must be positive")
	need(c.Outbox.PollInterval > 0, "outbox.poll_interval must be positive")
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// placeholder returns the path of the first string field that still holds a
// "<...>" placeholder from config.example.yaml.
func placeholder(v reflect.Value, path string) string {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			tag := strings.Split(v.Type().Field(i).Tag.Get("yaml"), ",")[0]
			if p := placeholder(v.Field(i), strings.TrimPrefix(path+"."+tag, ".")); p != "" {
				return p
			}
		}
	case reflect.String:
		s := strings.TrimSpace(v.String())
		if strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") {
			return path
		}
	}
	return ""
}

func validHost(h string) bool {
	if ip := net.ParseIP(h); ip != nil && ip.To4() != nil {
		return true
	}
	return len(h) <= 253 && dbHost.MatchString(h)
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func tlsURLs(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ",") {
		u, err := url.Parse(strings.TrimSpace(part))
		if err != nil || u.Scheme != "tls" || u.Host == "" {
			return false
		}
	}
	return true
}

// Load reads and parses the file at path. Environment variables play no part.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return Parse(data)
}

// DSN returns the PostgreSQL connection URL: verify-full TLS against the
// configured CA, with the search path set to the service's own schema.
func (d DB) DSN() string {
	q := url.Values{}
	q.Set("sslmode", d.SSLMode)
	q.Set("sslrootcert", d.CAFile)
	q.Set("search_path", d.Schema)
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(d.User, d.Password),
		Host:     d.Host + ":" + strconv.Itoa(d.Port),
		Path:     "/" + d.Name,
		RawQuery: q.Encode(),
	}
	return u.String()
}
