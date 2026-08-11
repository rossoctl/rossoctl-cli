package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/rossoctl-cli/internal/instances"
)

// instancesDir returns the directory a record in namespace lands in under the
// isolated HOME. It mirrors instances.Dir rather than calling it, so a test would
// notice if that function started resolving somewhere else.
func instancesDir(t *testing.T, namespace string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	return filepath.Join(home, ".config", "rossoctl", "namespaces", namespace)
}

// defaultInstancesDir is instancesDir for the namespace exec falls back to when
// neither --namespace nor a context supplies one. Most tests below are not about
// namespace selection and use this.
func defaultInstancesDir(t *testing.T) string {
	t.Helper()
	return instancesDir(t, instances.DefaultNamespace)
}

// readOnlyInstance reads the single instance file in dir, failing unless there is
// exactly one.
func readOnlyInstance(t *testing.T, dir string) instances.Instance {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			files = append(files, e.Name())
		}
	}
	if len(files) != 1 {
		t.Fatalf("found %v in %s, want exactly one instance file", files, dir)
	}

	data, err := os.ReadFile(filepath.Join(dir, files[0]))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var inst instances.Instance
	if err := json.Unmarshal(data, &inst); err != nil {
		t.Fatalf("unmarshal %s: %v", files[0], err)
	}
	return inst
}

// TestExecWritesInstanceFileWhileCommandRuns verifies the record exists for the
// duration of the command and is removed once exec returns.
//
// The child copies the file out to a path of its own before exiting, which is
// what proves the record was present *while* the command ran rather than merely
// written at some point: exec deletes the original on the way out, so a test that
// only looked afterwards could not tell the two apart.
func TestExecWritesInstanceFileWhileCommandRuns(t *testing.T) {
	isolateHome(t)
	dir := defaultInstancesDir(t)

	copyTo := filepath.Join(t.TempDir(), "seen.json")
	// Exit 21 if no record is present while the child runs.
	script := fmt.Sprintf(`set -e; f=$(ls %s/*.json 2>/dev/null | head -1);`+
		`[ -n "$f" ] || exit 21; cp "$f" %s`, dir, copyTo)

	if out, code := execExitCode(t, "authbridge", "exec",
		"--config", writeConfig(t, pipelineOnlyConfig(t)),
		"--", "sh", "-c", script); code != 0 {
		if code == 21 {
			t.Fatalf("no instance file existed while the command ran\n%s", out)
		}
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}

	// The copy the child made proves what the live record looked like.
	data, err := os.ReadFile(copyTo)
	if err != nil {
		t.Fatalf("the child did not capture an instance file: %v", err)
	}
	var inst instances.Instance
	if err := json.Unmarshal(data, &inst); err != nil {
		t.Fatalf("the captured record is not valid JSON: %v", err)
	}
	if inst.Name == "" {
		t.Error("the record has no name")
	}
	if inst.PID != os.Getpid() {
		t.Errorf("pid = %d, want the hosting process %d", inst.PID, os.Getpid())
	}

	// And the original is gone now that exec has returned.
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			t.Errorf("instance file %s survived exec", e.Name())
		}
	}
}

// TestExecInstanceFileRecordsCommandLine verifies the hosted command and its
// arguments are recorded as invoked, which is what makes a record identifiable.
func TestExecInstanceFileRecordsCommandLine(t *testing.T) {
	isolateHome(t)
	dir := defaultInstancesDir(t)

	// The child copies its own record aside so it can be read after exec has
	// deleted the original.
	copyDir := t.TempDir()
	script := fmt.Sprintf(`cp %s/*.json %s/`, dir, copyDir)

	if _, code := execExitCode(t, "authbridge", "exec",
		"--config", writeConfig(t, pipelineOnlyConfig(t)),
		"--", "sh", "-c", script, "extra-arg"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	inst := readOnlyInstance(t, copyDir)
	want := []string{"sh", "-c", script, "extra-arg"}
	if !slices.Equal(inst.CommandLine, want) {
		t.Errorf("command line = %v, want %v", inst.CommandLine, want)
	}
}

// TestExecInstanceFileRecordsAddresses verifies the record names the addresses
// the listeners actually bound.
//
// The config uses fixed loopback ports so the test knows what to expect. The
// admin address must be empty on this path: the in-process host starts no admin
// server, and reporting one would advertise something nothing is listening on.
func TestExecInstanceFileRecordsAddresses(t *testing.T) {
	isolateHome(t)
	dir := defaultInstancesDir(t)

	inbound := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	session := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg := writeConfig(t, fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [reverse]
  reverse_proxy_addr: %q
  reverse_proxy_backend: "http://127.0.0.1:1"
  session_api_addr: %q
pipeline:
  inbound:
    plugins:
      - name: inference-parser
`, inbound, session))

	copyDir := t.TempDir()
	script := fmt.Sprintf(`cp %s/*.json %s/`, dir, copyDir)

	if _, code := execExitCode(t, "authbridge", "exec",
		"--config", cfg, "--", "sh", "-c", script); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	inst := readOnlyInstance(t, copyDir)
	if inst.InboundAddr != inbound {
		t.Errorf("inbound = %q, want the bound reverse proxy address %q", inst.InboundAddr, inbound)
	}
	if inst.SessionAddr != session {
		t.Errorf("session = %q, want the bound session API address %q", inst.SessionAddr, session)
	}
	if inst.AdminAddr != "" {
		t.Errorf("admin = %q, want empty: the in-process host starts no admin server", inst.AdminAddr)
	}
	// Nothing ran in a container on this path.
	if inst.ContainerName != "" {
		t.Errorf("container name = %q, want empty on the in-process path", inst.ContainerName)
	}
}

// TestExecInstanceFileOmitsInboundWhenForwardOnly verifies an egress-only
// instance records no inbound address, since it has no inbound listener.
func TestExecInstanceFileOmitsInboundWhenForwardOnly(t *testing.T) {
	isolateHome(t)
	dir := defaultInstancesDir(t)

	copyDir := t.TempDir()
	script := fmt.Sprintf(`cp %s/*.json %s/`, dir, copyDir)

	if _, code := execExitCode(t, "authbridge", "exec",
		"--config", writeConfig(t, forwardConfig(t, forwardAddr(t), "")),
		"--", "sh", "-c", script); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	inst := readOnlyInstance(t, copyDir)
	if inst.InboundAddr != "" {
		t.Errorf("inbound = %q, want empty for a forward-only host", inst.InboundAddr)
	}
	// The session API is still on, so its address is still recorded.
	if inst.SessionAddr == "" {
		t.Error("session address should still be recorded for a forward-only host")
	}
}

// TestExecInstanceProtocolDefaultsToA2A verifies the default inbound protocol,
// which the AuthBridge config has no field to express.
func TestExecInstanceProtocolDefaultsToA2A(t *testing.T) {
	isolateHome(t)
	dir := defaultInstancesDir(t)

	copyDir := t.TempDir()
	script := fmt.Sprintf(`cp %s/*.json %s/`, dir, copyDir)

	if _, code := execExitCode(t, "authbridge", "exec",
		"--config", writeConfig(t, pipelineOnlyConfig(t)),
		"--", "sh", "-c", script); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	if got := readOnlyInstance(t, copyDir).InboundProtocol; got != instances.ProtocolA2A {
		t.Errorf("inbound protocol = %q, want the default %q", got, instances.ProtocolA2A)
	}
}

// TestExecInstanceProtocolOverride verifies --inboundProtocol mcp is recorded.
func TestExecInstanceProtocolOverride(t *testing.T) {
	isolateHome(t)
	dir := defaultInstancesDir(t)

	copyDir := t.TempDir()
	script := fmt.Sprintf(`cp %s/*.json %s/`, dir, copyDir)

	if _, code := execExitCode(t, "authbridge", "exec",
		"--config", writeConfig(t, pipelineOnlyConfig(t)),
		"--inboundProtocol", "mcp",
		"--", "sh", "-c", script); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	if got := readOnlyInstance(t, copyDir).InboundProtocol; got != instances.ProtocolMCP {
		t.Errorf("inbound protocol = %q, want %q", got, instances.ProtocolMCP)
	}
}

// TestExecRejectsUnknownInboundProtocol verifies a bad protocol fails before the
// command runs, rather than being recorded or silently defaulted.
//
// The child would exit 0, so a zero exit code here would mean it ran.
func TestExecRejectsUnknownInboundProtocol(t *testing.T) {
	isolateHome(t)

	_, err := execute(t, "authbridge", "exec",
		"--config", writeConfig(t, pipelineOnlyConfig(t)),
		"--inboundProtocol", "smtp",
		"--", "true")
	if err == nil {
		t.Fatal("an unknown --inboundProtocol should be rejected")
	}
	if !strings.Contains(err.Error(), "smtp") {
		t.Errorf("the error should name the rejected value: %v", err)
	}
}

// TestExecInstanceNameOverride verifies --instanceName is recorded instead of a
// generated name.
func TestExecInstanceNameOverride(t *testing.T) {
	isolateHome(t)
	dir := defaultInstancesDir(t)

	copyDir := t.TempDir()
	script := fmt.Sprintf(`cp %s/*.json %s/`, dir, copyDir)

	if _, code := execExitCode(t, "authbridge", "exec",
		"--config", writeConfig(t, pipelineOnlyConfig(t)),
		"--instanceName", "my-instance",
		"--", "sh", "-c", script); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	if got := readOnlyInstance(t, copyDir).Name; got != "my-instance" {
		t.Errorf("name = %q, want the supplied %q", got, "my-instance")
	}
}

// TestExecRejectsUnusableInstanceName verifies a name that could not be used as a
// file name is refused before the command runs, rather than being sanitized into
// something the operator did not ask for.
//
// The child would exit 0, so a zero exit code here would mean it ran.
func TestExecRejectsUnusableInstanceName(t *testing.T) {
	isolateHome(t)

	for _, name := range []string{"../escape", "sub/dir", "..", ".hidden", "has space"} {
		_, err := execute(t, "authbridge", "exec",
			"--config", writeConfig(t, pipelineOnlyConfig(t)),
			"--instanceName", name,
			"--", "true")
		if err == nil {
			t.Errorf("--instanceName %q should be rejected", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error for %q should name the rejected value: %v", name, err)
		}
	}
}

// TestExecRejectsDuplicateInstanceName verifies a name already claimed in the
// namespace is refused, so a second instance cannot displace a running one's
// record.
func TestExecRejectsDuplicateInstanceName(t *testing.T) {
	isolateHome(t)

	// Stand in for an instance that is already running under this name.
	h, err := instances.Create(instances.Instance{
		Namespace:   instances.DefaultNamespace,
		Name:        "taken",
		CommandLine: []string{"running"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = h.Remove() })

	_, err = execute(t, "authbridge", "exec",
		"--config", writeConfig(t, pipelineOnlyConfig(t)),
		"--instanceName", "taken",
		"--", "true")
	if err == nil {
		t.Fatal("a name already in use should be rejected")
	}
	if !strings.Contains(err.Error(), "taken") {
		t.Errorf("the error should name the clashing instance: %v", err)
	}

	// The existing record is untouched: the rejection must not have rewritten it.
	got, err := instances.Get(instances.DefaultNamespace, "taken")
	if err != nil {
		t.Fatalf("Get after the rejection: %v", err)
	}
	if !slices.Equal(got.CommandLine, []string{"running"}) {
		t.Errorf("command line = %v, want the original instance's", got.CommandLine)
	}
}

// TestExecDuplicateNameIsPerNamespace verifies the same name is usable again in a
// different namespace, since uniqueness is scoped to the namespace directory.
func TestExecDuplicateNameIsPerNamespace(t *testing.T) {
	isolateHome(t)

	h, err := instances.Create(instances.Instance{
		Namespace: "team1", Name: "shared", CommandLine: []string{"running"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = h.Remove() })

	dir := instancesDir(t, "team2")
	copyDir := t.TempDir()
	script := fmt.Sprintf(`cp %s/*.json %s/`, dir, copyDir)

	if out, code := execExitCode(t, "authbridge", "exec",
		"--config", writeConfig(t, pipelineOnlyConfig(t)),
		"--namespace", "team2",
		"--instanceName", "shared",
		"--", "sh", "-c", script); code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}

	if got := readOnlyInstance(t, copyDir).Name; got != "shared" {
		t.Errorf("name = %q, want %q", got, "shared")
	}
}

// TestExecNamespaceFlagChoosesTheDirectory verifies --namespace decides where the
// record is written, and is recorded in it.
func TestExecNamespaceFlagChoosesTheDirectory(t *testing.T) {
	isolateHome(t)
	dir := instancesDir(t, "chosen-ns")

	copyDir := t.TempDir()
	script := fmt.Sprintf(`cp %s/*.json %s/`, dir, copyDir)

	if out, code := execExitCode(t, "authbridge", "exec",
		"--config", writeConfig(t, pipelineOnlyConfig(t)),
		"--namespace", "chosen-ns",
		"--", "sh", "-c", script); code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}

	if got := readOnlyInstance(t, copyDir).Namespace; got != "chosen-ns" {
		t.Errorf("recorded namespace = %q, want %q", got, "chosen-ns")
	}
	// And nothing landed in the default namespace.
	if entries, err := os.ReadDir(defaultInstancesDir(t)); err == nil && len(entries) > 0 {
		t.Errorf("the default namespace holds %d entries, want none", len(entries))
	}
}

// TestExecNamespaceDefaultsToTheContext verifies the current context's namespace
// is used when --namespace is omitted, so a record lands where the operator's
// other commands are already pointed.
func TestExecNamespaceDefaultsToTheContext(t *testing.T) {
	isolateHome(t)

	if _, err := execute(t, "config", "create-context", "--name", "ctx",
		"--server", "https://example.invalid", "--namespace", "from-context"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	dir := instancesDir(t, "from-context")
	copyDir := t.TempDir()
	script := fmt.Sprintf(`cp %s/*.json %s/`, dir, copyDir)

	if out, code := execExitCode(t, "authbridge", "exec",
		"--config", writeConfig(t, pipelineOnlyConfig(t)),
		"--", "sh", "-c", script); code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}

	if got := readOnlyInstance(t, copyDir).Namespace; got != "from-context" {
		t.Errorf("recorded namespace = %q, want the context's %q", got, "from-context")
	}
}

// TestExecNamespaceFlagBeatsTheContext verifies an explicit --namespace wins over
// the context, which is the point of having the flag.
func TestExecNamespaceFlagBeatsTheContext(t *testing.T) {
	isolateHome(t)

	if _, err := execute(t, "config", "create-context", "--name", "ctx",
		"--server", "https://example.invalid", "--namespace", "from-context"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	dir := instancesDir(t, "from-flag")
	copyDir := t.TempDir()
	script := fmt.Sprintf(`cp %s/*.json %s/`, dir, copyDir)

	if out, code := execExitCode(t, "authbridge", "exec",
		"--config", writeConfig(t, pipelineOnlyConfig(t)),
		"--namespace", "from-flag",
		"--", "sh", "-c", script); code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}

	if got := readOnlyInstance(t, copyDir).Namespace; got != "from-flag" {
		t.Errorf("recorded namespace = %q, want the flag's %q", got, "from-flag")
	}
}

// TestExecNamespaceFallsBackWhenContextHasNone verifies a context with an empty
// namespace does not leave the record unplaced: it lands in the default namespace
// rather than in the base directory alongside the namespace directories.
func TestExecNamespaceFallsBackWhenContextHasNone(t *testing.T) {
	isolateHome(t)

	if _, err := execute(t, "config", "create-context", "--name", "ctx",
		"--server", "https://example.invalid"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	dir := defaultInstancesDir(t)
	copyDir := t.TempDir()
	script := fmt.Sprintf(`cp %s/*.json %s/`, dir, copyDir)

	if out, code := execExitCode(t, "authbridge", "exec",
		"--config", writeConfig(t, pipelineOnlyConfig(t)),
		"--", "sh", "-c", script); code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}

	if got := readOnlyInstance(t, copyDir).Namespace; got != instances.DefaultNamespace {
		t.Errorf("recorded namespace = %q, want the fallback %q", got, instances.DefaultNamespace)
	}
}

// TestExecRejectsUnusableNamespace verifies a --namespace that could escape the
// base directory is refused rather than resolved.
func TestExecRejectsUnusableNamespace(t *testing.T) {
	isolateHome(t)

	for _, ns := range []string{"../escape", "a/b", "..", ".hidden"} {
		_, err := execute(t, "authbridge", "exec",
			"--config", writeConfig(t, pipelineOnlyConfig(t)),
			"--namespace", ns,
			"--", "true")
		if err == nil {
			t.Errorf("--namespace %q should be rejected", ns)
			continue
		}
		if !strings.Contains(err.Error(), ns) {
			t.Errorf("the error for %q should name the rejected value: %v", ns, err)
		}
	}
}

// TestExecInstanceFileRemovedOnChildFailure verifies the record is cleaned up
// even when the hosted command fails — the file tracks whether an instance is
// running, not whether it succeeded.
func TestExecInstanceFileRemovedOnChildFailure(t *testing.T) {
	isolateHome(t)
	dir := defaultInstancesDir(t)

	if _, code := execExitCode(t, "authbridge", "exec",
		"--config", writeConfig(t, pipelineOnlyConfig(t)),
		"--", "sh", "-c", "exit 7"); code != 7 {
		t.Fatalf("exit code = %d, want the child's 7", code)
	}

	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			t.Errorf("instance file %s survived a failing command", e.Name())
		}
	}
}

// TestConcurrentInstancesGetDistinctFiles verifies two instances live at the
// same time are two records rather than one overwriting the other.
//
// This drives instances.Create directly rather than running two execs: what is
// under test is that concurrent records coexist, and a nested exec would add a
// subprocess and a second pipeline without testing anything more.
func TestConcurrentInstancesGetDistinctFiles(t *testing.T) {
	isolateHome(t)
	dir := defaultInstancesDir(t)

	first, err := instances.Create(instances.Instance{CommandLine: []string{"one"}})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := instances.Create(instances.Instance{CommandLine: []string{"two"}})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	t.Cleanup(func() {
		_ = first.Remove()
		_ = second.Remove()
	})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var count int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			count++
		}
	}
	if count != 2 {
		t.Errorf("%s holds %d records, want 2 — the second overwrote the first", dir, count)
	}

	// Removing one must not disturb the other: teardown of one instance cannot
	// deregister another.
	if err := first.Remove(); err != nil {
		t.Fatalf("Remove first: %v", err)
	}
	if _, err := os.Stat(second.Path); err != nil {
		t.Errorf("removing one record disturbed the other: %v", err)
	}
}

// TestInstanceFlagsAreDocumented verifies the new flags appear in help, so an
// operator can discover them.
func TestInstanceFlagsAreDocumented(t *testing.T) {
	isolateHome(t)

	out, err := execute(t, "authbridge", "exec", "--help")
	if err != nil {
		t.Fatalf("exec --help: %v", err)
	}
	for _, flag := range []string{"--instanceName", "--inboundProtocol", "--namespace"} {
		if !strings.Contains(out, flag) {
			t.Errorf("help does not document %s:\n%s", flag, out)
		}
	}
}
