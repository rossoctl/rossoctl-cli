package instances

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// isolateHome points the instances directory at a temp directory, mirroring the
// cmd package's helper of the same name so a test never touches the real
// ~/.config. It returns the directory a default-namespace instance file will
// land in.
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")
	return filepath.Join(dir, ".config", "rossocortex", "namespaces", DefaultNamespace)
}

// TestDirIsUnderRossocortex verifies the documented location, which is the one
// thing an external reader of the directory depends on.
func TestDirIsUnderRossocortex(t *testing.T) {
	want := isolateHome(t)

	got, err := Dir(DefaultNamespace)
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

// TestDirIsPerNamespace verifies each namespace gets its own directory under the
// shared base, which is what lets one namespace be scanned without the others.
func TestDirIsPerNamespace(t *testing.T) {
	isolateHome(t)

	base, err := BaseDir()
	if err != nil {
		t.Fatalf("BaseDir: %v", err)
	}
	for _, ns := range []string{"team1", "team2", "other"} {
		got, err := Dir(ns)
		if err != nil {
			t.Fatalf("Dir(%q): %v", ns, err)
		}
		if want := filepath.Join(base, ns); got != want {
			t.Errorf("Dir(%q) = %q, want %q", ns, got, want)
		}
	}
}

// TestDirEmptyNamespaceIsDefault verifies an unresolved namespace lands in the
// default namespace's directory rather than in the base directory, where the file
// would sit among the namespace directories.
func TestDirEmptyNamespaceIsDefault(t *testing.T) {
	isolateHome(t)

	got, err := Dir("")
	if err != nil {
		t.Fatalf("Dir(\"\"): %v", err)
	}
	want, err := Dir(DefaultNamespace)
	if err != nil {
		t.Fatalf("Dir(default): %v", err)
	}
	if got != want {
		t.Errorf("Dir(\"\") = %q, want the default namespace's %q", got, want)
	}
}

// TestDirRejectsTraversingNamespace verifies a namespace that would escape the
// base directory is refused rather than resolved.
func TestDirRejectsTraversingNamespace(t *testing.T) {
	isolateHome(t)

	for _, ns := range []string{"..", "../evil", "a/b", "."} {
		if got, err := Dir(ns); err == nil {
			t.Errorf("Dir(%q) = %q, want an error", ns, got)
		}
	}
}

// TestDirHonorsXDGConfigHome verifies $XDG_CONFIG_HOME takes precedence over
// $HOME, matching config.DefaultPath.
func TestDirHonorsXDGConfigHome(t *testing.T) {
	isolateHome(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	got, err := Dir("team9")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if want := filepath.Join(xdg, "rossocortex", "namespaces", "team9"); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

// TestCreateWritesRecord verifies Create writes a <name>.json file holding every
// field it was given.
func TestCreateWritesRecord(t *testing.T) {
	dir := isolateHome(t)

	in := Instance{
		Name:            "swift-falcon-abcd",
		ContainerName:   "rossoctl-authbridge-swift-falcon-abcd",
		InboundAddr:     "127.0.0.1:8080",
		InboundProtocol: ProtocolMCP,
		SessionAddr:     "127.0.0.1:54321",
		AdminAddr:       "127.0.0.1:54322",
		CommandLine:     []string{"python", "agent.py", "--flag"},
	}
	h, err := Create(in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The file is named for the instance and sits in its namespace's directory.
	if got, want := filepath.Dir(h.Path), dir; got != want {
		t.Errorf("file directory = %q, want %q", got, want)
	}
	if got, want := filepath.Base(h.Path), in.Name+".json"; got != want {
		t.Errorf("file name = %q, want %q", got, want)
	}
	if got := h.Instance.Namespace; got != DefaultNamespace {
		t.Errorf("namespace = %q, want the default %q", got, DefaultNamespace)
	}

	got, err := Load(h.Path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Name != in.Name {
		t.Errorf("name = %q, want %q", got.Name, in.Name)
	}
	if got.ContainerName != in.ContainerName {
		t.Errorf("container name = %q, want %q", got.ContainerName, in.ContainerName)
	}
	if got.InboundAddr != in.InboundAddr {
		t.Errorf("inbound = %q, want %q", got.InboundAddr, in.InboundAddr)
	}
	if got.InboundProtocol != ProtocolMCP {
		t.Errorf("protocol = %q, want %q", got.InboundProtocol, ProtocolMCP)
	}
	if got.SessionAddr != in.SessionAddr {
		t.Errorf("session = %q, want %q", got.SessionAddr, in.SessionAddr)
	}
	if got.AdminAddr != in.AdminAddr {
		t.Errorf("admin = %q, want %q", got.AdminAddr, in.AdminAddr)
	}
	if !slices.Equal(got.CommandLine, in.CommandLine) {
		t.Errorf("command line = %v, want %v", got.CommandLine, in.CommandLine)
	}
	if got.PID != os.Getpid() {
		t.Errorf("pid = %d, want this process %d", got.PID, os.Getpid())
	}
}

// TestCreateGeneratesIDAndName verifies the identity fields are filled in when
// left blank, and reported back on the handle so a caller need not re-read them.
func TestCreateGeneratesIDAndName(t *testing.T) {
	isolateHome(t)

	h, err := Create(Instance{CommandLine: []string{"true"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	uuidRE := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRE.MatchString(h.Instance.ID) {
		t.Errorf("id = %q, want a version-4 UUID", h.Instance.ID)
	}
	if h.Instance.Name == "" {
		t.Error("name was not generated")
	}
	// The default protocol applies rather than an empty string reaching the file.
	if h.Instance.InboundProtocol != DefaultProtocol {
		t.Errorf("protocol = %q, want the default %q", h.Instance.InboundProtocol, DefaultProtocol)
	}
}

// TestCreateHonorsSuppliedName verifies an explicit name is not overwritten by a
// generated one.
func TestCreateHonorsSuppliedName(t *testing.T) {
	isolateHome(t)

	h, err := Create(Instance{Name: "chosen", CommandLine: []string{"true"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if h.Instance.Name != "chosen" {
		t.Errorf("name = %q, want the supplied %q", h.Instance.Name, "chosen")
	}
}

// TestCreateIDsAreDistinct verifies two instances do not collide on one file,
// which would make the second silently replace the first.
func TestCreateIDsAreDistinct(t *testing.T) {
	isolateHome(t)

	a, err := Create(Instance{CommandLine: []string{"a"}})
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := Create(Instance{CommandLine: []string{"b"}})
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}
	if a.Instance.ID == b.Instance.ID {
		t.Fatalf("both instances got id %q", a.Instance.ID)
	}
	if a.Path == b.Path {
		t.Errorf("both instances wrote %q", a.Path)
	}
}

// TestCreateOmitsAbsentAddresses verifies the optional address fields are absent
// from the JSON rather than emitted as empty strings, so a reader can tell "no
// such listener" from a blank.
func TestCreateOmitsAbsentAddresses(t *testing.T) {
	isolateHome(t)

	// An egress-only instance: no inbound listener, no session API, no admin.
	h, err := Create(Instance{CommandLine: []string{"curl", "example.com"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	data, err := os.ReadFile(h.Path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"container_name", "inbound_addr", "session_addr", "admin_addr"} {
		if _, ok := raw[k]; ok {
			t.Errorf("key %q should be omitted when empty, got %v", k, raw[k])
		}
	}
	// The protocol is recorded even with no inbound listener: it documents what
	// the instance would front.
	if raw["inbound_protocol"] != string(DefaultProtocol) {
		t.Errorf("inbound_protocol = %v, want %q", raw["inbound_protocol"], DefaultProtocol)
	}
}

// TestCreateDirectoryIsPrivate verifies the instances directory is not
// world-readable: its records name unauthenticated session API ports.
func TestCreateDirectoryIsPrivate(t *testing.T) {
	dir := isolateHome(t)

	if _, err := Create(Instance{CommandLine: []string{"true"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %#o, want 0700", perm)
	}
}

// TestCreateLeavesNoTempFiles verifies the write-and-rename leaves only the final
// record, so a reader scanning the directory sees no partial files.
func TestCreateLeavesNoTempFiles(t *testing.T) {
	dir := isolateHome(t)

	if _, err := Create(Instance{CommandLine: []string{"true"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want exactly the one record", names)
	}
	if strings.HasSuffix(entries[0].Name(), ".tmp") {
		t.Errorf("a temp file was left behind: %q", entries[0].Name())
	}
}

// TestRemoveDeletesFile verifies the record goes away on shutdown.
func TestRemoveDeletesFile(t *testing.T) {
	isolateHome(t)

	h, err := Create(Instance{CommandLine: []string{"true"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(h.Path); !os.IsNotExist(err) {
		t.Errorf("file should be gone, stat err = %v", err)
	}
}

// TestRemoveIsIdempotent verifies a second Remove is not an error, so a shutdown
// path reachable more than once does not have to guard against it.
func TestRemoveIsIdempotent(t *testing.T) {
	isolateHome(t)

	h, err := Create(Instance{CommandLine: []string{"true"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.Remove(); err != nil {
		t.Fatalf("first Remove: %v", err)
	}
	if err := h.Remove(); err != nil {
		t.Errorf("second Remove should be a no-op, got %v", err)
	}
}

// TestRemoveNilHandle verifies a nil handle is a no-op, so `defer h.Remove()`
// is safe after a Create that failed.
func TestRemoveNilHandle(t *testing.T) {
	var h *Handle
	if err := h.Remove(); err != nil {
		t.Errorf("Remove on a nil handle should be a no-op, got %v", err)
	}
}

// TestListReportsCreated verifies List finds the records that exist and stops
// reporting one that has been removed.
func TestListReportsCreated(t *testing.T) {
	isolateHome(t)

	a, err := Create(Instance{Name: "one", CommandLine: []string{"a"}})
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	if _, err := Create(Instance{Name: "two", CommandLine: []string{"b"}}); err != nil {
		t.Fatalf("Create b: %v", err)
	}

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d instances, want 2", len(got))
	}

	if err := a.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, err = List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List returned %d instances after a removal, want 1", len(got))
	}
	if got[0].Name != "two" {
		t.Errorf("remaining instance = %q, want %q", got[0].Name, "two")
	}
}

// TestListMissingDirectoryIsEmpty verifies a directory that was never created
// reports no instances rather than an error — nothing has ever run.
func TestListMissingDirectoryIsEmpty(t *testing.T) {
	isolateHome(t)

	got, err := List()
	if err != nil {
		t.Fatalf("List on a missing directory should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %v, want no instances", got)
	}
}

// TestListSkipsUnparseableAndNonJSON verifies one corrupt record does not hide
// the valid ones, and that unrelated files are ignored.
func TestListSkipsUnparseableAndNonJSON(t *testing.T) {
	dir := isolateHome(t)

	if _, err := Create(Instance{Name: "good", CommandLine: []string{"a"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write broken: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List returned %d instances, want just the valid one", len(got))
	}
	if got[0].Name != "good" {
		t.Errorf("instance = %q, want %q", got[0].Name, "good")
	}
}

// TestParseProtocol covers the accepted spellings, the default, and the
// rejection of an unknown value.
func TestParseProtocol(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Protocol
		wantErr bool
	}{
		{in: "a2a", want: ProtocolA2A},
		{in: "mcp", want: ProtocolMCP},
		{in: "A2A", want: ProtocolA2A},
		{in: "MCP", want: ProtocolMCP},
		{in: " mcp ", want: ProtocolMCP},
		// Unset means the default rather than an error.
		{in: "", want: DefaultProtocol},
		{in: "   ", want: DefaultProtocol},
		// A typo is reported rather than silently becoming a2a.
		{in: "a2", wantErr: true},
		{in: "http", wantErr: true},
	} {
		got, err := ParseProtocol(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseProtocol(%q) should error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseProtocol(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseProtocol(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDefaultProtocolIsA2A pins the documented default.
func TestDefaultProtocolIsA2A(t *testing.T) {
	if DefaultProtocol != ProtocolA2A {
		t.Errorf("DefaultProtocol = %q, want %q", DefaultProtocol, ProtocolA2A)
	}
}

// TestNewNameShape verifies generated names are adjective-noun-suffix and vary
// between calls, which is what makes them usable as labels in a listing.
func TestNewNameShape(t *testing.T) {
	nameRE := regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9a-f]{4}$`)

	seen := map[string]bool{}
	for range 50 {
		got, err := NewName()
		if err != nil {
			t.Fatalf("NewName: %v", err)
		}
		if !nameRE.MatchString(got) {
			t.Fatalf("NewName() = %q, want adjective-noun-hhhh", got)
		}
		seen[got] = true
	}
	// Not a uniqueness guarantee, but 50 identical names would mean the
	// randomness is not reaching the name at all.
	if len(seen) < 2 {
		t.Errorf("NewName returned %d distinct names over 50 calls", len(seen))
	}
}

// TestNewContainerNameDerivesFromInstance verifies the container name carries the
// instance name, which is what lets a reader match `docker ps` to a record.
func TestNewContainerNameDerivesFromInstance(t *testing.T) {
	got := NewContainerName("swift-falcon-3f9c")
	if !strings.Contains(got, "swift-falcon-3f9c") {
		t.Errorf("NewContainerName(...) = %q, want it to contain the instance name", got)
	}
	if !strings.HasPrefix(got, "rossoctl-") {
		t.Errorf("NewContainerName(...) = %q, want a rossoctl- prefix", got)
	}
}

// TestProtocolValid verifies only the two known protocols are accepted.
func TestProtocolValid(t *testing.T) {
	for _, p := range []Protocol{ProtocolA2A, ProtocolMCP} {
		if !p.Valid() {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range []Protocol{"", "grpc", "A2A"} {
		if p.Valid() {
			t.Errorf("%q should not be valid", p)
		}
	}
}
