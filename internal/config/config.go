// Package config manages rossoctl's persisted context configuration, stored
// in ~/.config/rossoctl/config.yaml (kubectl-style).
//
// A Config holds a list of named contexts — each with a server URI and an
// optional bearer token — plus the name of the current context. Like the
// other internal packages it is free of Cobra: callers pass an explicit file
// path so it can be unit-tested against a temporary directory.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	// dirPerm and filePerm are the required permissions for the config
	// directory and file: private to the owner (the file may hold a token).
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// Type records what kind of server a Context targets: a hosted Rossoctl API
// ("api") or a local Cortex ("cortex").
//
// Both are reached the same way, over HTTP at the context's server URI, so the
// type does not select a client implementation — it is a label describing what
// is at the other end.
type Type string

const (
	// TypeAPI is a context served by the Rossoctl HTTP API (backed by a
	// Kubernetes cluster).
	TypeAPI Type = "api"
	// TypeCortex is a context served by a local Cortex, as started by
	// `rossoctl cortex serve`.
	TypeCortex Type = "cortex"
)

// Context is a single named target: a type, a server URI, an optional default
// namespace, and an optional bearer token.
type Context struct {
	Name        string `yaml:"name" json:"name"`
	Type        Type   `yaml:"type,omitempty" json:"type,omitempty"`
	Server      string `yaml:"server" json:"server"`
	Namespace   string `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	BearerToken string `yaml:"bearerToken,omitempty" json:"bearerToken,omitempty"`
}

// Config is the on-disk configuration: the set of contexts and which one is
// current.
type Config struct {
	CurrentContext string    `yaml:"currentContext" json:"currentContext"`
	Contexts       []Context `yaml:"contexts" json:"contexts"`

	// path is where this Config was loaded from / will be saved to. It is not
	// serialized.
	path string `yaml:"-" json:"-"`
}

// DefaultPath returns the default config file location,
// ~/.config/rossoctl/config.yaml. The base directory honors $XDG_CONFIG_HOME
// (defaulting to ~/.config) so it follows the XDG Base Directory spec.
func DefaultPath() (string, error) {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating home directory: %w", err)
		}
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "rossoctl", "config.yaml"), nil
}

// Load reads and parses the config at path. A missing file is not an error:
// an empty Config (with path remembered) is returned so the caller can seed
// and Save it. A present but unreadable or malformed file is an error.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{path: path}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	cfg := &Config{path: path}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	cfg.path = path // Unmarshal doesn't touch the unexported field, but be explicit.
	return cfg, nil
}

// Save writes the config back to its path, creating the parent directory
// (0700) and the file (0600) if needed. Existing files are chmod'd to 0600 so
// they conform even if they predated this code.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config has no path to save to")
	}

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	// Ensure the directory has the required perms even if it already existed.
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("setting permissions on %s: %w", dir, err)
	}

	if err := os.WriteFile(c.path, data, filePerm); err != nil {
		return fmt.Errorf("writing %s: %w", c.path, err)
	}
	// WriteFile only applies perms when creating; chmod an existing file too.
	if err := os.Chmod(c.path, filePerm); err != nil {
		return fmt.Errorf("setting permissions on %s: %w", c.path, err)
	}
	return nil
}

// Path returns the file this config is bound to.
func (c *Config) Path() string { return c.path }

// Get returns the named context, or false if there is none.
func (c *Config) Get(name string) (*Context, bool) {
	for i := range c.Contexts {
		if c.Contexts[i].Name == name {
			return &c.Contexts[i], true
		}
	}
	return nil, false
}

// Current returns the current context, or false if none is set or it is
// missing from the list.
func (c *Config) Current() (*Context, bool) {
	if c.CurrentContext == "" {
		return nil, false
	}
	return c.Get(c.CurrentContext)
}

// Upsert adds ctx, replacing any existing context with the same name.
func (c *Config) Upsert(ctx Context) {
	for i := range c.Contexts {
		if c.Contexts[i].Name == ctx.Name {
			c.Contexts[i] = ctx
			return
		}
	}
	c.Contexts = append(c.Contexts, ctx)
}

// SetCurrent makes name the current context. It errors if name is unknown.
func (c *Config) SetCurrent(name string) error {
	if _, ok := c.Get(name); !ok {
		return fmt.Errorf("no context named %q", name)
	}
	c.CurrentContext = name
	return nil
}

// Delete removes the named context. It errors if name is unknown. If the
// deleted context was current, the current-context reference is cleared.
func (c *Config) Delete(name string) error {
	for i := range c.Contexts {
		if c.Contexts[i].Name == name {
			c.Contexts = append(c.Contexts[:i], c.Contexts[i+1:]...)
			if c.CurrentContext == name {
				c.CurrentContext = ""
			}
			return nil
		}
	}
	return fmt.Errorf("no context named %q", name)
}

// Rename changes the name of context oldName to newName. It errors if oldName
// is unknown or newName already names a different context. Renaming a no-op
// (oldName == newName) is allowed. If the renamed context was current, the
// current-context reference is updated to newName.
func (c *Config) Rename(oldName, newName string) error {
	if newName == "" {
		return fmt.Errorf("new context name must not be empty")
	}
	ctx, ok := c.Get(oldName)
	if !ok {
		return fmt.Errorf("no context named %q", oldName)
	}
	if oldName == newName {
		return nil
	}
	if _, exists := c.Get(newName); exists {
		return fmt.Errorf("a context named %q already exists", newName)
	}
	ctx.Name = newName
	if c.CurrentContext == oldName {
		c.CurrentContext = newName
	}
	return nil
}

// ContextNameForServer derives a short context name from a server URI: its
// host (without port). It falls back to the host:port, and finally to the raw
// string, when the URI can't be parsed or has no host — so the name is never
// empty.
func ContextNameForServer(server string) string {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return server
	}
	if h := u.Hostname(); h != "" {
		return h
	}
	return u.Host
}

// EnsureContext loads the config at path and, if it contains no contexts,
// seeds one from defaultServer (named after the server's hostname, with the
// full URI as its server and an empty bearer token), makes it current, and
// saves. The resulting config is returned. This is the single place the lazy
// create-if-missing behavior lives.
func EnsureContext(path, defaultServer string) (*Config, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	if len(cfg.Contexts) == 0 {
		name := ContextNameForServer(defaultServer)
		cfg.Upsert(Context{Name: name, Type: TypeAPI, Server: defaultServer})
		if err := cfg.SetCurrent(name); err != nil {
			return nil, err
		}
		if err := cfg.Save(); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}
