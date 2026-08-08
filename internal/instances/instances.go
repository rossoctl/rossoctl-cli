// Package instances records running AuthBridge instances as files on disk.
//
// `authbridge exec` hosts a proxy pipeline around a child command. While that
// child runs, the ports it is reachable on are only known to the process that
// bound them: the session API and admin endpoints land on ephemeral ports, and
// a container's published ports differ on every run. Nothing else on the host
// can find them.
//
// An instance file closes that gap. Each running instance writes one
// <name>.json into its namespace's directory on startup and removes it on
// shutdown, so the set of files present is the set of instances running. A
// separate tool — or a person with `cat` — can read a file to learn where to
// reach an instance and what command it is hosting.
//
// Records are grouped by namespace: ~/.config/rossocortex/namespaces/<ns>/.
// The namespace is a directory rather than a field inside the file so the set
// of namespaces can be listed without opening every record, and so one
// namespace's instances can be scanned without reading the others'.
//
// The file is named for the instance rather than its UUID, which makes a record
// addressable by the name a listing shows — but also makes the name part of a
// path. Names and namespaces are therefore validated (see ValidName): a name
// that could escape its directory or collide with a sibling is rejected at the
// point it enters the package, not sanitized into something else.
//
// The record is advisory, not authoritative. A process killed with SIGKILL
// never runs its cleanup, so a stale file can outlive the instance it
// describes. Readers should treat a file as a claim to be verified (by
// connecting to one of its addresses) rather than as proof of a live process.
// Recording a PID is what makes that verification possible without a
// connection, which is why PID is part of the record.
//
// Like the other internal packages this one is free of Cobra: Create takes a
// fully-populated Instance and the caller decides what goes in it.
package instances

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Protocol is the application protocol an instance's inbound listener speaks.
type Protocol string

const (
	// ProtocolA2A is the Agent-to-Agent protocol, the default.
	ProtocolA2A Protocol = "a2a"

	// ProtocolMCP is the Model Context Protocol.
	ProtocolMCP Protocol = "mcp"
)

// DefaultProtocol is the inbound protocol assumed when none is given. The
// AuthBridge config has no field naming the protocol its reverse proxy fronts,
// so it cannot be derived — a2a is the assumption, and callers that know better
// set InboundProtocol explicitly.
const DefaultProtocol = ProtocolA2A

// Valid reports whether p is a protocol this package knows.
func (p Protocol) Valid() bool {
	return p == ProtocolA2A || p == ProtocolMCP
}

// ParseProtocol interprets a protocol name, case-insensitively. An empty string
// yields DefaultProtocol; anything else unrecognized is an error rather than a
// silent fallback, so a typo is reported instead of quietly becoming a2a.
func ParseProtocol(s string) (Protocol, error) {
	if strings.TrimSpace(s) == "" {
		return DefaultProtocol, nil
	}
	p := Protocol(strings.ToLower(strings.TrimSpace(s)))
	if !p.Valid() {
		return "", fmt.Errorf("unknown inbound protocol %q: want %q or %q", s, ProtocolA2A, ProtocolMCP)
	}
	return p, nil
}

// Instance describes one running AuthBridge instance.
//
// The address fields hold host:port strings as reached from *this host*, not as
// bound inside a container: for a container-hosted instance they are the
// published ephemeral host ports, which is what a caller on this host can
// actually connect to. Each is omitted when the instance has no such listener,
// so a reader can distinguish "not listening" from "listening on an unknown
// port" — the latter would be a bug worth seeing rather than a blank.
type Instance struct {
	// ID is a unique identifier for this run, generated fresh each time. The
	// record's identity on disk is its namespace and name, so this is not what
	// addresses it; it distinguishes two runs that reused a name, which matters
	// to anything correlating logs across restarts. Create fills it if empty.
	ID string `json:"id"`

	// Name is a human-readable handle for the instance, and the stem of its file
	// name. Create fills it with a generated name if empty, so an instance
	// started without one is still recorded and addressable.
	Name string `json:"name"`

	// Namespace is the namespace the instance is recorded in, and the directory
	// its file lives in. It is stored in the record as well as implied by the
	// path so a file read on its own — by `cat`, or after being copied — still
	// says where it belongs.
	Namespace string `json:"namespace"`

	// ContainerName is the proxy container's name, empty when the pipeline runs
	// in the rossoctl process rather than a container.
	ContainerName string `json:"container_name,omitempty"`

	// InboundAddr is where callers reach the hosted service — the reverse
	// proxy. Empty when the config has no inbound listener, which is the case
	// for an egress-only (forward-role) instance.
	InboundAddr string `json:"inbound_addr,omitempty"`

	// InboundProtocol is what InboundAddr speaks. It is recorded even without
	// an inbound listener, where it documents what the instance would front.
	InboundProtocol Protocol `json:"inbound_protocol"`

	// SessionAddr is the session events endpoint (the 9094 listener). Empty
	// when the session API is disabled.
	SessionAddr string `json:"session_addr,omitempty"`

	// AdminAddr is the stats/config endpoint (the 9093 listener). Empty when no
	// admin endpoint is reachable — including on the in-process path, which
	// does not start one.
	AdminAddr string `json:"admin_addr,omitempty"`

	// CommandLine is the hosted command and its arguments, as invoked.
	CommandLine []string `json:"command_line"`

	// PID is the process that wrote this record — the rossoctl process hosting
	// the instance, not the child. It lets a reader test whether a file is
	// stale without opening a connection.
	PID int `json:"pid"`
}

// DefaultNamespace is the namespace used when none is given. Instances are
// local processes with no namespace of their own, so one has to be assumed
// rather than derived; callers that know better pass a namespace explicitly.
const DefaultNamespace = "team1"

// BaseDir returns the directory holding the per-namespace instance
// directories, ~/.config/rossocortex/namespaces.
//
// The base directory honors $XDG_CONFIG_HOME (defaulting to ~/.config) so it
// follows the XDG Base Directory spec, matching how the rossoctl config file
// itself is located. Note the "rossocortex" component: instance files describe
// cortex instances rather than rossoctl's own state, so they sit beside the
// rossoctl directory rather than inside it.
func BaseDir() (string, error) {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating home directory: %w", err)
		}
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "rossocortex", "namespaces"), nil
}

// Dir returns the directory holding namespace's instance records.
//
// An empty namespace yields DefaultNamespace's directory rather than the base
// directory itself, so a caller that forgot to resolve a namespace writes
// somewhere valid instead of scattering records among the namespace directories.
func Dir(namespace string) (string, error) {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	if err := ValidName(namespace); err != nil {
		return "", fmt.Errorf("invalid namespace: %w", err)
	}
	base, err := BaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, namespace), nil
}

// maxNameLen bounds a name so it cannot exceed what a filesystem accepts as a
// single component. 255 bytes is the common limit; the ".json" suffix and the
// temp-file decoration Create adds have to fit inside it too.
const maxNameLen = 200

// ValidName reports whether s is usable as a namespace or instance name, which
// is to say as a single path component.
//
// Names reach this package from --instanceName and --namespace and become path
// components, so they are checked rather than trusted: "..", a name containing a
// separator, or a leading dot would respectively escape the directory, write
// outside it, or hide the record from a listing that skips dotfiles. The
// permitted set is deliberately narrow — letters, digits, dot, dash and
// underscore — because a name is a handle a person types and reads, and anything
// needing quoting to appear in a shell or a URL is a poor handle whatever the
// filesystem would tolerate.
func ValidName(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("name is empty")
	case len(s) > maxNameLen:
		return fmt.Errorf("name is %d bytes, longer than the %d-byte limit", len(s), maxNameLen)
	case s == "." || s == "..":
		return fmt.Errorf("name %q is a directory reference", s)
	case strings.HasPrefix(s, "."):
		// A dotfile would be skipped by the listing scan, so the record would
		// exist but never be reported — worse than refusing to write it.
		return fmt.Errorf("name %q starts with a dot", s)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("name %q contains %q; use letters, digits, '-', '_' or '.'", s, r)
		}
	}
	return nil
}

// Handle is a written instance file. Remove deletes it.
type Handle struct {
	// Path is the file that was written.
	Path string

	// Instance is what was written to it, including any field Create filled in.
	Instance Instance
}

// Create writes inst as a JSON file in its namespace's directory and returns a
// handle for removing it.
//
// Empty ID, Name, Namespace, InboundProtocol, and PID fields are filled in: the
// ID and Name are generated, the namespace and protocol default, and the PID is
// this process. The returned handle carries the completed record, so a caller can
// report the generated name without regenerating it.
//
// An existing record with the same name in the same namespace is an error rather
// than an overwrite: the file name is the instance's identity now, so writing
// over one would make a running instance unreachable and would be undone when
// the loser shut down and removed "its" file. Callers wanting to know in advance
// can ask Exists, but this check is the authoritative one — it is the only one
// that cannot be raced, since O_EXCL makes the test and the creation a single
// operation.
//
// The record is written to a temp file and linked into place rather than renamed,
// because rename would clobber an existing file and lose exactly the collision
// this needs to report. A reader scanning the directory therefore never sees a
// half-written record. The directory is created if it does not exist.
func Create(inst Instance) (*Handle, error) {
	if inst.Namespace == "" {
		inst.Namespace = DefaultNamespace
	}
	dir, err := Dir(inst.Namespace)
	if err != nil {
		return nil, err
	}

	if inst.ID == "" {
		id, err := NewID()
		if err != nil {
			return nil, err
		}
		inst.ID = id
	}
	if inst.Name == "" {
		name, err := NewName()
		if err != nil {
			return nil, err
		}
		inst.Name = name
	}
	if err := ValidName(inst.Name); err != nil {
		return nil, fmt.Errorf("invalid instance name: %w", err)
	}
	if inst.InboundProtocol == "" {
		inst.InboundProtocol = DefaultProtocol
	}
	if inst.PID == 0 {
		inst.PID = os.Getpid()
	}

	// 0o700: the record names ports serving an unauthenticated session API,
	// which is a hint worth keeping to this user rather than publishing to every
	// account on the host.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating namespace directory %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding instance record: %w", err)
	}
	data = append(data, '\n')

	path := filepath.Join(dir, inst.Name+".json")

	tmp, err := os.CreateTemp(dir, "."+inst.Name+".*.tmp")
	if err != nil {
		return nil, fmt.Errorf("creating instance file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// From here a failure must not leave the temp file behind.
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, fmt.Errorf("writing instance file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("closing instance file %s: %w", tmpName, err)
	}
	// Link, not rename: it fails rather than replacing an existing file, which
	// is what makes the duplicate-name check race-free.
	if err := os.Link(tmpName, path); err != nil {
		os.Remove(tmpName)
		if errors.Is(err, fs.ErrExist) {
			return nil, &DuplicateError{Namespace: inst.Namespace, Name: inst.Name, Path: path}
		}
		return nil, fmt.Errorf("linking instance file into %s: %w", path, err)
	}
	// The temp name was only ever a staging handle; the record lives at path.
	os.Remove(tmpName)

	return &Handle{Path: path, Instance: inst}, nil
}

// DuplicateError reports that an instance of the same name is already recorded
// in the namespace.
//
// It is a distinct type rather than a formatted error so a caller can recognize
// the case — `authbridge exec` reports a name clash differently from a directory
// it cannot write.
type DuplicateError struct {
	Namespace string
	Name      string
	Path      string
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("an instance named %q already exists in namespace %q (%s)",
		e.Name, e.Namespace, e.Path)
}

// Exists reports whether an instance of this name is recorded in the namespace.
//
// This is advisory: an instance can appear or disappear between the check and
// whatever follows it. Create makes the same check atomically and is what
// actually prevents a duplicate; Exists is for reporting a clash early, before a
// caller has done expensive setup work it would have to undo.
func Exists(namespace, name string) (bool, error) {
	dir, err := Dir(namespace)
	if err != nil {
		return false, err
	}
	if err := ValidName(name); err != nil {
		return false, fmt.Errorf("invalid instance name: %w", err)
	}
	_, err = os.Stat(filepath.Join(dir, name+".json"))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("checking for an existing instance %q: %w", name, err)
}

// Remove deletes the instance file. An already-absent file is not an error, so
// Remove is safe to call twice — a shutdown path that runs on several exit
// routes does not need to coordinate.
//
// A nil handle is a no-op, so a caller can defer Remove on the result of a
// Create that may have failed.
func (h *Handle) Remove() error {
	if h == nil || h.Path == "" {
		return nil
	}
	if err := os.Remove(h.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing instance file %s: %w", h.Path, err)
	}
	return nil
}

// Load reads one instance file.
//
// The returned error wraps the underlying one, so a caller can test a missing
// file with errors.Is(err, fs.ErrNotExist) — Get relies on that to distinguish an
// absent instance from an unreadable one.
func Load(path string) (*Instance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var inst Instance
	if err := json.Unmarshal(data, &inst); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &inst, nil
}

// Namespaces returns the namespaces that have a directory, sorted.
//
// A missing base directory yields none rather than an error: nothing has ever
// run, which is a legitimate state rather than a failure. Only directories are
// reported, and dotted ones are skipped along with any stray file, so a
// half-written temp file cannot masquerade as a namespace.
func Namespaces() ([]string, error) {
	base, err := BaseDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading namespaces directory %s: %w", base, err)
	}

	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out, nil
}

// ListNamespace reads every instance file in one namespace's directory.
//
// A missing directory yields no instances rather than an error: that namespace
// has nothing running, which is a legitimate state. Files that cannot be read or
// parsed are skipped, so one corrupt record does not hide the rest.
//
// The namespace of each returned instance is set from the directory it was found
// in, so a record whose stored namespace disagrees with its location is reported
// where it actually lives. The same applies to the name: the file name is the
// instance's identity, so it wins over a stale field inside the file.
func ListNamespace(namespace string) ([]Instance, error) {
	dir, err := Dir(namespace)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading namespace directory %s: %w", dir, err)
	}

	var out []Instance
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		inst, err := Load(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		inst.Namespace = namespace
		inst.Name = strings.TrimSuffix(e.Name(), ".json")
		out = append(out, *inst)
	}
	return out, nil
}

// List reads every instance file in every namespace directory.
//
// Namespaces are visited in sorted order and instances within one in directory
// order, so a listing is stable across calls rather than reordering between
// refreshes.
//
// A namespace directory that cannot be read is skipped rather than failing the
// whole listing: one unreadable namespace should not make every other namespace's
// instances invisible. A failure to read the base directory is still an error,
// since that means nothing can be listed at all.
func List() ([]Instance, error) {
	namespaces, err := Namespaces()
	if err != nil {
		return nil, err
	}

	var out []Instance
	for _, ns := range namespaces {
		insts, err := ListNamespace(ns)
		if err != nil {
			continue
		}
		out = append(out, insts...)
	}
	return out, nil
}

// Get reads the instance named name in namespace.
//
// A missing record is reported as fs.ErrNotExist (wrapped), so a caller can tell
// "no such instance" from "the record is there but unreadable" — the first is a
// 404 to an API caller and the second is a 500.
func Get(namespace, name string) (*Instance, error) {
	dir, err := Dir(namespace)
	if err != nil {
		return nil, err
	}
	if err := ValidName(name); err != nil {
		return nil, fmt.Errorf("invalid instance name: %w", err)
	}

	inst, err := Load(filepath.Join(dir, name+".json"))
	if err != nil {
		return nil, err
	}
	// The location is authoritative over the file's own fields; see ListNamespace.
	inst.Namespace = namespace
	inst.Name = name
	return inst, nil
}

// NewID returns a random RFC 4122 version 4 UUID in the usual hyphenated form.
//
// It is generated here rather than taken from a UUID library to avoid a direct
// dependency for what is a dozen lines: crypto/rand supplies the randomness,
// and the version and variant bits are set per the spec.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating instance id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122

	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// nameAdjectives and nameNouns are the halves of a generated instance name.
// Two short word lists give adjective-noun-suffix names that are easy to say
// and to tell apart in a listing, which a raw UUID is not.
var (
	nameAdjectives = []string{
		"amber", "brisk", "calm", "clever", "dapper", "eager", "fleet", "gentle",
		"jolly", "keen", "lucid", "merry", "nimble", "placid", "quiet", "rapid",
		"steady", "sunny", "swift", "tidy", "vivid", "warm", "witty", "zesty",
	}
	nameNouns = []string{
		"anchor", "beacon", "bridge", "canyon", "cedar", "comet", "delta", "ember",
		"falcon", "harbor", "island", "jasper", "lagoon", "meadow", "nebula", "orbit",
		"pebble", "quartz", "ridge", "summit", "tundra", "valley", "willow", "zephyr",
	}
)

// NewName returns a randomly generated instance name, such as
// "swift-falcon-3f9c".
//
// The four-hex-digit suffix is what keeps names distinct: the word lists alone
// give only a few hundred combinations, so two concurrent instances would
// collide often enough to be confusing. The suffix is not a uniqueness
// guarantee, and the name is now the record's file name — so a collision is
// possible and Create reports it rather than overwriting. Names are generated
// from the same alphabet ValidName permits, so a generated name is always usable
// as a file name.
func NewName() (string, error) {
	// Three bytes: one to pick each word, one for the suffix's high half. The
	// suffix's four hex digits come from the last two bytes.
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating instance name: %w", err)
	}
	adj := nameAdjectives[int(b[0])%len(nameAdjectives)]
	noun := nameNouns[int(b[1])%len(nameNouns)]
	return fmt.Sprintf("%s-%s-%s", adj, noun, hex.EncodeToString(b[2:])), nil
}

// NewContainerName returns a container name derived from an instance name, such
// as "rossoctl-authbridge-swift-falcon-3f9c".
//
// The prefix makes rossoctl's containers identifiable in `docker ps` output
// alongside everything else running, and matching the instance name is what
// lets a reader connect a container back to the record describing it.
func NewContainerName(instanceName string) string {
	return "rossoctl-authbridge-" + instanceName
}
