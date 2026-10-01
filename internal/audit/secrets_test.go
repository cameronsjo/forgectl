package audit

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// writeMode writes content at root/rel with mode (chmod'd past the umask).
func writeMode(t *testing.T, root, rel, content string, mode fs.FileMode) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func scanSecrets(t *testing.T, root string) SecretsReport {
	t.Helper()
	r, err := ScanSecrets(SecretsOptions{Root: root})
	if err != nil {
		t.Fatalf("ScanSecrets: %v", err)
	}
	return r
}

func secretsByPath(r SecretsReport) map[string]SecretFinding {
	m := make(map[string]SecretFinding, len(r.Findings))
	for _, f := range r.Findings {
		m[f.Path] = f
	}
	return m
}

const pemKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\n"

// TestClassifySecret pins the name rules: which basenames are .env files,
// keys, and scanner config, and that matching folds ASCII case only.
//
// Mutations that turn it red: drop any entry of envTemplateExts (its
// template row then matches); replace asciiLower with strings.ToLower (the
// Kelvin-sign row then matches .key); drop the .pub exclusion by matching
// keyNames on a prefix; move sniffExts into keyExts (cert.pem no longer
// needs the sniff).
func TestClassifySecret(t *testing.T) {
	cases := []struct {
		name  string
		kind  string
		sniff bool
	}{
		{".env", KindEnv, false},
		{".ENV", KindEnv, false},
		{".envrc", KindEnv, false},
		{".env.local", KindEnv, false},
		{".env.Production", KindEnv, false},
		{".env.example", "", false},
		{".env.SAMPLE", "", false},
		{".env.template", "", false},
		{".env.tmpl", "", false},
		{".env.dist", "", false},
		{".env.local.example", "", false},
		{".env.example.local", KindEnv, false},
		{"prod.env", "", false},
		{".environment", "", false},
		{"env", "", false},
		{"id_rsa", KindKey, false},
		{"ID_ED25519", KindKey, false},
		{"id_dsa", KindKey, false},
		{"id_ecdsa", KindKey, false},
		{"id_ecdsa_sk", KindKey, false},
		{"id_ed25519_sk", KindKey, false},
		{"id_rsa.pub", "", false},
		{"id_rsa_backup", "", false},
		{"store.p12", KindKey, false},
		{"store.PFX", KindKey, false},
		{"release.keystore", KindKey, false},
		{"cert.pem", KindKey, true},
		{"server.KEY", KindKey, true},
		{"server.Key", "", false}, // the Kelvin sign lowercases to k in Unicode, not in ASCII
		{".gitleaks.toml", KindScannerConfig, false},
		{".GitleaksIgnore", KindScannerConfig, false},
		{"gitleaks.toml", "", false},
	}
	for _, c := range cases {
		kind, sniff := classifySecret(c.name)
		if kind != c.kind || sniff != c.sniff {
			t.Errorf("classifySecret(%q) = %q,%v; want %q,%v", c.name, kind, sniff, c.kind, c.sniff)
		}
	}
}

// TestScanSecrets_FindsByNameAndSniff covers the native scan end to end on
// a real tree: kinds, types, the sniff deciding .pem/.key, the outside-repo
// and vendored flags, repo attribution, a directory named .env walked into
// rather than reported, and .git never entered.
func TestScanSecrets_FindsByNameAndSniff(t *testing.T) {
	root := t.TempDir()
	repo := mkrepo(t, root, "app")
	writeMode(t, repo, ".env", "A=1\n", 0o600)
	writeMode(t, repo, ".env.example", "A=\n", 0o600)
	writeMode(t, repo, "deploy/id_rsa", pemKey, 0o600)
	writeMode(t, repo, "deploy/id_rsa.pub", "ssh-rsa AAAA\n", 0o644)
	writeMode(t, repo, "tls/server.key", pemKey, 0o600)
	writeMode(t, repo, "tls/cert.pem", "-----BEGIN CERTIFICATE-----\nMIIB\n", 0o600)
	writeMode(t, repo, "tls/empty.key", "", 0o600)
	writeMode(t, repo, "android/release.keystore", "\x00\x01", 0o600)
	writeMode(t, repo, ".gitleaksignore", "x:y:1\n", 0o600)
	writeMode(t, repo, "node_modules/pkg/.env", "B=2\n", 0o600)
	writeMode(t, repo, ".git/.env", "C=3\n", 0o600)
	writeMode(t, repo, "py/.env/lib/id_ed25519", pemKey, 0o600) // a virtualenv named .env
	writeMode(t, root, "loose/.envrc", "export X=1\n", 0o600)

	r := scanSecrets(t, root)
	got := secretsByPath(r)
	want := map[string]struct {
		kind, repo string
		flags      []string
	}{
		filepath.Join(repo, ".env"):                     {KindEnv, repo, nil},
		filepath.Join(repo, "deploy/id_rsa"):            {KindKey, repo, nil},
		filepath.Join(repo, "tls/server.key"):           {KindKey, repo, nil},
		filepath.Join(repo, "android/release.keystore"): {KindKey, repo, nil},
		filepath.Join(repo, ".gitleaksignore"):          {KindScannerConfig, repo, nil},
		filepath.Join(repo, "node_modules/pkg/.env"):    {KindEnv, repo, []string{FlagVendored}},
		filepath.Join(repo, "py/.env/lib/id_ed25519"):   {KindKey, repo, nil},
		filepath.Join(root, "loose/.envrc"):             {KindEnv, "", []string{FlagOutsideRepo}},
	}
	for p, w := range want {
		f, ok := got[p]
		if !ok {
			t.Errorf("missing finding %s", p)
			continue
		}
		if f.Kind != w.kind || f.Repo != w.repo || f.Type != TypeFile || !slices.Equal(f.Flags, append([]string{}, w.flags...)) {
			t.Errorf("%s = %+v, want kind %s repo %q type file flags %v", p, f, w.kind, w.repo, w.flags)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected finding %s", p)
		}
	}
	if !slices.Equal(r.Repos, []string{repo}) {
		t.Errorf("repos = %v, want [%s]", r.Repos, repo)
	}
}

// TestScanSecrets_SymlinksReportedUnread: a link with a matching name is
// reported as type symlink and never followed or read, wherever it points.
// A link named *.pem is reported unread too, since the sniff never follows
// one. Mutation that turns it red: sniff a symlinked *.pem (the out-of-root
// target then fails the confined open and the link drops out).
func TestScanSecrets_SymlinksReportedUnread(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	outside := t.TempDir()
	secret := writeMode(t, outside, "real_key", pemKey, 0o644)
	root := t.TempDir()
	repo := mkrepo(t, root, "r")
	for _, name := range []string{".env", "id_rsa", "outside.pem"} {
		if err := os.Symlink(secret, filepath.Join(repo, name)); err != nil {
			t.Fatal(err)
		}
	}
	sniffed := 0
	r, err := scanSecretsWithRoot(t, root, func(ops *fsOps) {
		inner := ops.sniff
		ops.sniff = func(name string) (bool, error) { sniffed++; return inner(name) }
	})
	if err != nil {
		t.Fatal(err)
	}
	got := secretsByPath(r)
	for _, name := range []string{".env", "id_rsa", "outside.pem"} {
		f, ok := got[filepath.Join(repo, name)]
		if !ok || f.Type != TypeSymlink {
			t.Errorf("%s: got %+v, want a symlink finding", name, f)
		}
		if slices.Contains(f.Flags, FlagLoose) {
			t.Errorf("%s: a link's mode is not the target's; loose must not be claimed", name)
		}
	}
	if sniffed != 0 {
		t.Errorf("sniff ran %d times; a symlink is never read", sniffed)
	}
}

// scanSecretsWithRoot opens root and runs the scan with ops adjusted by
// adjust, as the effective uid this process has.
func scanSecretsWithRoot(t *testing.T, root string, adjust func(*fsOps)) (SecretsReport, error) {
	t.Helper()
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	ops := rootOps(r)
	if adjust != nil {
		adjust(&ops)
	}
	return scanSecretsWith(root, ops, SecretsOptions{}, currentEUID())
}

// TestScanSecrets_SniffIsCapped pins the content read's bound: a private-key
// header inside the first sniffBytes is seen, one starting past it is not.
//
// Mutation that turns it red: raise sniffBytes, or read with io.ReadAll.
func TestScanSecrets_SniffIsCapped(t *testing.T) {
	root := t.TempDir()
	marker := string(privateKeyMarker)
	inside := strings.Repeat("x", sniffBytes-len(marker)) + marker
	past := strings.Repeat("x", sniffBytes+1) + "-----BEGIN RSA " + marker
	writeMode(t, root, "inside.pem", inside, 0o600)
	writeMode(t, root, "past.pem", past, 0o600)
	got := secretsByPath(scanSecrets(t, root))
	if _, ok := got[filepath.Join(root, "inside.pem")]; !ok {
		t.Error("a header ending exactly at the sniff cap was missed")
	}
	if _, ok := got[filepath.Join(root, "past.pem")]; ok {
		t.Error("a header past the sniff cap was seen: the read is not capped")
	}
}

// TestScanSecrets_SniffOnlyForPemAndKey: the sniff is asked about regular
// *.pem/*.key files and nothing else, and a failed sniff is counted in
// UnreadableFiles, not listed. Mutation that turns it red: sniff every
// key-kind match (id_rsa is then sniffed too).
func TestScanSecrets_SniffOnlyForPemAndKey(t *testing.T) {
	root := t.TempDir()
	writeMode(t, root, "a.pem", pemKey, 0o600)
	writeMode(t, root, "b.key", pemKey, 0o600)
	writeMode(t, root, "id_rsa", pemKey, 0o600)
	writeMode(t, root, ".env", "A=1", 0o600)
	writeMode(t, root, "notes.txt", pemKey, 0o600)
	var asked []string
	r, err := scanSecretsWithRoot(t, root, func(ops *fsOps) {
		ops.sniff = func(name string) (bool, error) {
			asked = append(asked, name)
			return false, fs.ErrPermission
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(asked)
	if !slices.Equal(asked, []string{"a.pem", "b.key"}) {
		t.Errorf("sniffed %v, want only the .pem and .key files", asked)
	}
	if r.UnreadableFiles != 2 {
		t.Errorf("unreadable_files = %d, want the 2 failed sniffs counted", r.UnreadableFiles)
	}
	if _, ok := secretsByPath(r)[filepath.Join(root, "a.pem")]; ok {
		t.Error("an unreadable .pem was listed as a key")
	}
}

// TestScanSecrets_LooseModeTable pins the loose rule: any group or other bit
// (mode & 0o077), ssh's rule for a private key, on keys and .env files
// alike; never on scanner config.
//
// Mutation that turns it red: narrow the mask to 0o007 (the 0640 and 0610
// rows then read tight), or to 0o044.
func TestScanSecrets_LooseModeTable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are synthesized on windows")
	}
	cases := []struct {
		mode  fs.FileMode
		loose bool
	}{
		{0o600, false}, {0o400, false}, {0o700, false},
		{0o640, true}, {0o604, true}, {0o610, true}, {0o602, true}, {0o644, true}, {0o660, true},
	}
	for _, c := range cases {
		root := t.TempDir()
		writeMode(t, root, "id_ed25519", pemKey, c.mode)
		writeMode(t, root, ".env", "A=1", c.mode)
		writeMode(t, root, ".gitleaks.toml", "", c.mode)
		got := secretsByPath(scanSecrets(t, root))
		for _, name := range []string{"id_ed25519", ".env"} {
			if l := slices.Contains(got[filepath.Join(root, name)].Flags, FlagLoose); l != c.loose {
				t.Errorf("%s at %#o: loose = %v, want %v", name, c.mode, l, c.loose)
			}
		}
		if slices.Contains(got[filepath.Join(root, ".gitleaks.toml")].Flags, FlagLoose) {
			t.Errorf("scanner config at %#o flagged loose", c.mode)
		}
	}
}

// TestScanSecrets_FIFONamedLikeASecret: a FIFO named .env is reported as
// type other and never opened; one named *.pem is not read and not listed.
// Neither blocks the scan.
func TestScanSecrets_FIFONamedLikeASecret(t *testing.T) {
	root := t.TempDir()
	if err := mkfifo(filepath.Join(root, ".env")); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := mkfifo(filepath.Join(root, "x.pem")); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	got := secretsByPath(scanSecrets(t, root))
	if f, ok := got[filepath.Join(root, ".env")]; !ok || f.Type != TypeOther {
		t.Errorf(".env FIFO: got %+v, want a type-other finding", f)
	}
	if _, ok := got[filepath.Join(root, "x.pem")]; ok {
		t.Error("a FIFO named x.pem was listed")
	}
}

// TestScanSecrets_FindingsCap: the findings cap stops the scan and says so.
func TestScanSecrets_FindingsCap(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a", "b", "c"} {
		writeMode(t, root, d+"/.env", "A=1", 0o600)
	}
	r, err := ScanSecrets(SecretsOptions{Root: root, MaxFindings: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Findings) != 2 || !r.Truncated || !slices.Equal(r.CappedBy, []string{CapFindings}) || !r.Stopped() {
		t.Errorf("findings=%d truncated=%v capped_by=%v, want 2/true/[findings]", len(r.Findings), r.Truncated, r.CappedBy)
	}
}

// TestApplyGitStatus pins how git's answer shapes the list: tracked files
// are flagged, an untracked .env no rule ignores is flagged unignored, an
// ignored .env is counted and dropped, an ignored key stays listed, a repo
// git cannot answer for keeps its findings flagged git-unknown, scanner
// config is never asked about, one call is made per repo, and flags come
// out in their fixed order.
//
// Mutations that turn it red: drop an ignored key as well (the key row
// disappears); treat a status error as "all ignored" (the b/.env row
// disappears); skip the case-folded lookup (.ENV reads ignored).
func TestApplyGitStatus(t *testing.T) {
	r := SecretsReport{Findings: []SecretFinding{
		{Path: "/p/a/.env", Repo: "/p/a", Kind: KindEnv, Flags: []string{FlagLoose}, repoRel: ".env"},
		{Path: "/p/a/.env.local", Repo: "/p/a", Kind: KindEnv, Flags: []string{}, repoRel: ".env.local"},
		{Path: "/p/a/sub/.env", Repo: "/p/a", Kind: KindEnv, Flags: []string{FlagVendored}, repoRel: "sub/.env"},
		{Path: "/p/a/.ENV.prod", Repo: "/p/a", Kind: KindEnv, Flags: []string{}, repoRel: ".ENV.prod"},
		{Path: "/p/a/id_rsa", Repo: "/p/a", Kind: KindKey, Flags: []string{FlagLoose}, repoRel: "id_rsa"},
		{Path: "/p/a/key.pem", Repo: "/p/a", Kind: KindKey, Flags: []string{}, repoRel: "key.pem"},
		{Path: "/p/a/.gitleaks.toml", Repo: "/p/a", Kind: KindScannerConfig, Flags: []string{}, repoRel: ".gitleaks.toml"},
		{Path: "/p/b/.env", Repo: "/p/b", Kind: KindEnv, Flags: []string{}, repoRel: ".env"},
		{Path: "/p/loose/.env", Kind: KindEnv, Flags: []string{FlagOutsideRepo}},
	}}
	calls := map[string][]string{}
	r.ApplyGitStatus(func(repo string, rels []string) (map[string]GitState, error) {
		calls[repo] = append(calls[repo], rels...)
		if repo == "/p/b" {
			return nil, errors.New("git failed")
		}
		return map[string]GitState{
			".env":      GitTracked,
			"sub/.env":  GitUntracked,
			"id_rsa":    GitTracked,
			".env.prod": GitTracked, // the index's spelling of .ENV.prod
		}, nil
	})
	if len(calls) != 2 || !slices.Equal(calls["/p/a"], []string{".env", ".env.local", "sub/.env", ".ENV.prod", "id_rsa", "key.pem"}) {
		t.Errorf("status calls = %v, want one per repo, scanner config excluded", calls)
	}
	want := map[string][]string{
		"/p/a/.env":           {FlagTracked, FlagLoose},
		"/p/a/sub/.env":       {FlagUnignored, FlagVendored},
		"/p/a/.ENV.prod":      {FlagTracked},
		"/p/a/id_rsa":         {FlagTracked, FlagLoose},
		"/p/a/key.pem":        {},
		"/p/a/.gitleaks.toml": {},
		"/p/b/.env":           {FlagGitUnknown},
		"/p/loose/.env":       {FlagOutsideRepo},
	}
	got := map[string][]string{}
	for _, f := range r.Findings {
		got[f.Path] = f.Flags
	}
	for p, flags := range want {
		g, ok := got[p]
		if !ok || !slices.Equal(g, flags) {
			t.Errorf("%s: flags %v (present %v), want %v", p, g, ok, flags)
		}
	}
	if len(got) != len(want) {
		t.Errorf("findings = %v, want exactly %d (the ignored .env.local dropped)", got, len(want))
	}
	if r.IgnoredEnv != 1 || r.GitStatusFailed != 1 {
		t.Errorf("ignored=%d git_failed=%d, want 1/1", r.IgnoredEnv, r.GitStatusFailed)
	}
}

// TestScanSecrets_RepoRelIsTheGitPathspec: the path ApplyGitStatus asks git
// about is relative to the finding's own repo, nested repos included.
func TestScanSecrets_RepoRelIsTheGitPathspec(t *testing.T) {
	root := t.TempDir()
	outer := mkrepo(t, root, "outer")
	inner := mkrepo(t, outer, "libs/inner")
	writeMode(t, outer, "cfg/.env", "", 0o600)
	writeMode(t, inner, "x/.env", "", 0o600)
	writeMode(t, root, ".env", "", 0o600)
	r := scanSecrets(t, root)
	asked := map[string][]string{}
	r.ApplyGitStatus(func(repo string, rels []string) (map[string]GitState, error) {
		asked[repo] = rels
		return map[string]GitState{}, nil
	})
	if !slices.Equal(asked[outer], []string{"cfg/.env"}) || !slices.Equal(asked[inner], []string{"x/.env"}) || len(asked) != 2 {
		t.Errorf("asked %v, want each .env relative to its own repo", asked)
	}
	if !slices.Equal(r.Repos, []string{outer, inner}) {
		t.Errorf("repos = %v, want both, sorted", r.Repos)
	}
}
