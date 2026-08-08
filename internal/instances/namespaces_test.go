package instances

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// createIn is Create for a named instance in a namespace, failing the test on
// error. Most tests below care about where records land rather than what is in
// them, so the rest of the record is boilerplate.
func createIn(t *testing.T, namespace, name string) *Handle {
	t.Helper()
	h, err := Create(Instance{
		Namespace:   namespace,
		Name:        name,
		CommandLine: []string{"true"},
	})
	if err != nil {
		t.Fatalf("Create(%s/%s): %v", namespace, name, err)
	}
	return h
}

// instNames returns "namespace/name" for each instance, for order-sensitive
// comparison.
func instNames(insts []Instance) []string {
	out := make([]string, 0, len(insts))
	for _, i := range insts {
		out = append(out, i.Namespace+"/"+i.Name)
	}
	return out
}

// TestCreateWritesIntoItsNamespace verifies the namespace decides the directory,
// so two same-named instances in different namespaces are different records.
func TestCreateWritesIntoItsNamespace(t *testing.T) {
	isolateHome(t)

	a := createIn(t, "team1", "shared-name")
	b := createIn(t, "team2", "shared-name")

	if a.Path == b.Path {
		t.Fatalf("both namespaces wrote %q", a.Path)
	}
	for _, tc := range []struct {
		h  *Handle
		ns string
	}{{a, "team1"}, {b, "team2"}} {
		wantDir, err := Dir(tc.ns)
		if err != nil {
			t.Fatalf("Dir(%q): %v", tc.ns, err)
		}
		if got := filepath.Dir(tc.h.Path); got != wantDir {
			t.Errorf("%s record is in %q, want %q", tc.ns, got, wantDir)
		}
		if got := tc.h.Instance.Namespace; got != tc.ns {
			t.Errorf("recorded namespace = %q, want %q", got, tc.ns)
		}
	}
}

// TestCreateRecordsNamespaceInTheFile verifies the namespace is in the JSON as
// well as the path, so a record read on its own still says where it belongs.
func TestCreateRecordsNamespaceInTheFile(t *testing.T) {
	isolateHome(t)

	h := createIn(t, "team7", "solo")
	got, err := Load(h.Path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Namespace != "team7" {
		t.Errorf("namespace in file = %q, want %q", got.Namespace, "team7")
	}
}

// TestCreateRejectsDuplicateName verifies a second instance of the same name in
// the same namespace is refused rather than overwriting the first — the file name
// is the record's identity, so an overwrite would make the running instance
// unreachable.
func TestCreateRejectsDuplicateName(t *testing.T) {
	isolateHome(t)

	first := createIn(t, "team1", "taken")

	_, err := Create(Instance{Namespace: "team1", Name: "taken", CommandLine: []string{"second"}})
	if err == nil {
		t.Fatal("Create with a taken name should fail")
	}

	var dup *DuplicateError
	if !errors.As(err, &dup) {
		t.Fatalf("error is %T (%v), want a *DuplicateError so a caller can recognize it", err, err)
	}
	if dup.Name != "taken" || dup.Namespace != "team1" {
		t.Errorf("DuplicateError = %+v, want name=taken namespace=team1", dup)
	}
	// The error should name the instance, so the operator knows what clashed.
	if !strings.Contains(err.Error(), "taken") {
		t.Errorf("error %q does not name the instance", err)
	}

	// The first record is intact: still one file, still the original command.
	got, err := Load(first.Path)
	if err != nil {
		t.Fatalf("Load after the rejected duplicate: %v", err)
	}
	if !slices.Equal(got.CommandLine, []string{"true"}) {
		t.Errorf("command line = %v, want the first instance's; the duplicate overwrote it", got.CommandLine)
	}
}

// TestCreateLeavesNoTempFileAfterDuplicate verifies the rejected attempt cleans
// up after itself, rather than leaving its staging file in the directory.
func TestCreateLeavesNoTempFileAfterDuplicate(t *testing.T) {
	dir := isolateHome(t)

	createIn(t, DefaultNamespace, "taken")
	if _, err := Create(Instance{Name: "taken", CommandLine: []string{"x"}}); err == nil {
		t.Fatal("duplicate should have failed")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var leftovers []string
	for _, e := range entries {
		if e.Name() != "taken.json" {
			leftovers = append(leftovers, e.Name())
		}
	}
	if len(leftovers) > 0 {
		t.Errorf("directory holds %v, want only taken.json", leftovers)
	}
}

// TestCreateSameNameDifferentNamespaceIsAllowed verifies the collision check is
// scoped to a namespace rather than global.
func TestCreateSameNameDifferentNamespaceIsAllowed(t *testing.T) {
	isolateHome(t)

	createIn(t, "team1", "dup")
	if _, err := Create(Instance{Namespace: "team2", Name: "dup", CommandLine: []string{"x"}}); err != nil {
		t.Errorf("same name in another namespace should be allowed: %v", err)
	}
}

// TestCreateReusesAFreedName verifies a name is available again once its instance
// has shut down, so a stable name can be used across restarts.
func TestCreateReusesAFreedName(t *testing.T) {
	isolateHome(t)

	h := createIn(t, "team1", "cycled")
	if err := h.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := Create(Instance{Namespace: "team1", Name: "cycled", CommandLine: []string{"again"}}); err != nil {
		t.Errorf("name should be free after the first instance was removed: %v", err)
	}
}

// TestCreateRejectsUnusableNames verifies a name that could escape its directory
// or hide from a listing is refused at the point it enters the package.
func TestCreateRejectsUnusableNames(t *testing.T) {
	isolateHome(t)

	for _, name := range []string{
		"..",
		".",
		"../escape",
		"sub/dir",
		`back\slash`,
		".hidden",
		"has space",
		"quote'name",
		strings.Repeat("x", maxNameLen+1),
	} {
		if _, err := Create(Instance{Name: name, CommandLine: []string{"x"}}); err == nil {
			t.Errorf("Create with name %q should fail", name)
		}
	}
}

// TestCreateRejectsUnusableNamespaces verifies the same check applies to the
// namespace, which is also a path component.
func TestCreateRejectsUnusableNamespaces(t *testing.T) {
	isolateHome(t)

	for _, ns := range []string{"..", "../escape", "a/b", ".hidden"} {
		if _, err := Create(Instance{Namespace: ns, Name: "ok", CommandLine: []string{"x"}}); err == nil {
			t.Errorf("Create in namespace %q should fail", ns)
		}
	}
}

// TestCreateEscapeAttemptWritesNothingOutside verifies a traversing name is not
// merely reported but leaves nothing behind above the namespace directory.
func TestCreateEscapeAttemptWritesNothingOutside(t *testing.T) {
	isolateHome(t)

	base, err := BaseDir()
	if err != nil {
		t.Fatalf("BaseDir: %v", err)
	}
	if _, err := Create(Instance{Name: "../escaped", CommandLine: []string{"x"}}); err == nil {
		t.Fatal("traversing name should have been refused")
	}
	if _, err := os.Stat(filepath.Join(base, "escaped.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file escaped into %s", base)
	}
}

// TestValidNameAcceptsGeneratedNames verifies NewName only produces names the
// validator accepts, so a generated name can always be written.
func TestValidNameAcceptsGeneratedNames(t *testing.T) {
	for range 200 {
		name, err := NewName()
		if err != nil {
			t.Fatalf("NewName: %v", err)
		}
		if err := ValidName(name); err != nil {
			t.Fatalf("generated name %q is not valid: %v", name, err)
		}
	}
}

// TestValidNameAcceptsOrdinaryNames verifies the permitted set is not so narrow
// that reasonable names are refused.
func TestValidNameAcceptsOrdinaryNames(t *testing.T) {
	for _, name := range []string{
		"team1", "my-agent", "my_agent", "agent.v2", "Agent1", "a",
		strings.Repeat("x", maxNameLen),
	} {
		if err := ValidName(name); err != nil {
			t.Errorf("ValidName(%q) = %v, want it accepted", name, err)
		}
	}
}

// TestExistsTracksTheRecord verifies Exists reports a name as taken only while
// its record is present.
func TestExistsTracksTheRecord(t *testing.T) {
	isolateHome(t)

	if got, err := Exists("team1", "ghost"); err != nil || got {
		t.Fatalf("Exists before creation = (%v, %v), want (false, nil)", got, err)
	}

	h := createIn(t, "team1", "ghost")
	if got, err := Exists("team1", "ghost"); err != nil || !got {
		t.Errorf("Exists after creation = (%v, %v), want (true, nil)", got, err)
	}
	// Scoped to the namespace.
	if got, err := Exists("team2", "ghost"); err != nil || got {
		t.Errorf("Exists in another namespace = (%v, %v), want (false, nil)", got, err)
	}

	if err := h.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got, err := Exists("team1", "ghost"); err != nil || got {
		t.Errorf("Exists after removal = (%v, %v), want (false, nil)", got, err)
	}
}

// TestNamespacesListsDirectories verifies the namespaces are discovered from the
// directories present, sorted, ignoring stray files.
func TestNamespacesListsDirectories(t *testing.T) {
	isolateHome(t)

	createIn(t, "team2", "b")
	createIn(t, "team1", "a")
	createIn(t, "alpha", "c")

	base, err := BaseDir()
	if err != nil {
		t.Fatalf("BaseDir: %v", err)
	}
	// A stray file beside the directories is not a namespace.
	if err := os.WriteFile(filepath.Join(base, "stray.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write stray: %v", err)
	}

	got, err := Namespaces()
	if err != nil {
		t.Fatalf("Namespaces: %v", err)
	}
	want := []string{"alpha", "team1", "team2"}
	if !slices.Equal(got, want) {
		t.Errorf("Namespaces = %v, want %v", got, want)
	}
}

// TestNamespacesMissingBaseIsEmpty verifies a base directory that was never
// created reports none rather than an error.
func TestNamespacesMissingBaseIsEmpty(t *testing.T) {
	isolateHome(t)

	got, err := Namespaces()
	if err != nil {
		t.Fatalf("Namespaces on a missing base should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Namespaces = %v, want none", got)
	}
}

// TestListSpansNamespaces verifies List reports instances from every namespace,
// each carrying the namespace it was found in.
func TestListSpansNamespaces(t *testing.T) {
	isolateHome(t)

	createIn(t, "team2", "second")
	createIn(t, "team1", "first")
	createIn(t, "team1", "also-first")

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Namespaces in sorted order; within one, directory order (which is sorted
	// for ReadDir).
	want := []string{"team1/also-first", "team1/first", "team2/second"}
	if !slices.Equal(instNames(got), want) {
		t.Errorf("List = %v, want %v", instNames(got), want)
	}
}

// TestListNamespaceIsScoped verifies one namespace's listing does not report
// another's.
func TestListNamespaceIsScoped(t *testing.T) {
	isolateHome(t)

	createIn(t, "team1", "mine")
	createIn(t, "team2", "theirs")

	got, err := ListNamespace("team1")
	if err != nil {
		t.Fatalf("ListNamespace: %v", err)
	}
	if !slices.Equal(instNames(got), []string{"team1/mine"}) {
		t.Errorf("ListNamespace(team1) = %v, want just team1/mine", instNames(got))
	}
}

// TestListNamespaceMissingIsEmpty verifies a namespace with no directory reports
// nothing rather than failing.
func TestListNamespaceMissingIsEmpty(t *testing.T) {
	isolateHome(t)

	got, err := ListNamespace("never-used")
	if err != nil {
		t.Fatalf("ListNamespace on a missing namespace should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListNamespace = %v, want none", got)
	}
}

// TestListPrefersTheLocationOverTheFile verifies a record whose stored namespace
// or name disagrees with its path is reported where it actually lives — the path
// is the identity, so a stale or hand-edited field cannot misdirect a listing.
func TestListPrefersTheLocationOverTheFile(t *testing.T) {
	isolateHome(t)

	dir, err := Dir("team1")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Hand-written record claiming to be somewhere it is not.
	body := `{"id":"x","name":"claimed-name","namespace":"claimed-ns","inbound_protocol":"a2a"}`
	if err := os.WriteFile(filepath.Join(dir, "real-name.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Equal(instNames(got), []string{"team1/real-name"}) {
		t.Errorf("List = %v, want team1/real-name from the path", instNames(got))
	}
}

// TestListSkipsTempFiles verifies a record being staged by a concurrent Create is
// not reported as an instance.
func TestListSkipsTempFiles(t *testing.T) {
	dir := isolateHome(t)

	createIn(t, DefaultNamespace, "real")
	// Create's staging files are dotted; they end in .tmp, but a dotted .json
	// would be the harder case, so use that.
	if err := os.WriteFile(filepath.Join(dir, ".staging.json"), []byte(`{"name":"x"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Equal(instNames(got), []string{DefaultNamespace + "/real"}) {
		t.Errorf("List = %v, want only the real record", instNames(got))
	}
}

// TestGetReadsOneRecord verifies Get returns the named instance with its fields.
func TestGetReadsOneRecord(t *testing.T) {
	isolateHome(t)

	if _, err := Create(Instance{
		Namespace:       "team1",
		Name:            "target",
		InboundAddr:     "127.0.0.1:8080",
		InboundProtocol: ProtocolMCP,
		CommandLine:     []string{"npx", "server"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := Get("team1", "target")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "target" || got.Namespace != "team1" {
		t.Errorf("got %s/%s, want team1/target", got.Namespace, got.Name)
	}
	if got.InboundAddr != "127.0.0.1:8080" {
		t.Errorf("inbound = %q, want the recorded address", got.InboundAddr)
	}
	if got.InboundProtocol != ProtocolMCP {
		t.Errorf("protocol = %q, want mcp", got.InboundProtocol)
	}
	if !slices.Equal(got.CommandLine, []string{"npx", "server"}) {
		t.Errorf("command line = %v, want the recorded one", got.CommandLine)
	}
}

// TestGetMissingIsNotExist verifies an absent record is distinguishable from an
// unreadable one, which is what lets a server answer 404 rather than 500.
func TestGetMissingIsNotExist(t *testing.T) {
	isolateHome(t)

	createIn(t, "team1", "present")

	for _, tc := range []struct{ ns, name string }{
		{"team1", "absent"},    // namespace exists, record does not
		{"nosuchns", "absent"}, // namespace does not exist either
	} {
		_, err := Get(tc.ns, tc.name)
		if err == nil {
			t.Fatalf("Get(%s/%s) should fail", tc.ns, tc.name)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Get(%s/%s) error = %v, want it to wrap fs.ErrNotExist", tc.ns, tc.name, err)
		}
	}
}

// TestGetRejectsTraversal verifies Get cannot be talked into reading a file
// outside the namespace directory — the name arrives from a URL path.
func TestGetRejectsTraversal(t *testing.T) {
	isolateHome(t)

	base, err := BaseDir()
	if err != nil {
		t.Fatalf("BaseDir: %v", err)
	}
	// A file one level up that a traversing name would reach.
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	secret := filepath.Join(base, "secret.json")
	if err := os.WriteFile(secret, []byte(`{"name":"secret"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, name := range []string{"../secret", "..", "sub/secret", ".hidden"} {
		if got, err := Get("team1", name); err == nil {
			t.Errorf("Get(team1/%q) returned %+v, want an error", name, got)
		}
	}
}
