package pinit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/initcfg"
)

func writeFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const basePasswd = "root:x:0:0:root:/root:/bin/bash\nnode:x:1000:1000::/home/node:/bin/bash\n"

const baseGroup = "root:x:0:\ndialout:x:20:\nnode:x:1000:\n"

func TestEnsureAccount(t *testing.T) {
	agent := account{"doug", "doug", 501, 20, "/Users/doug", agentShell}
	tests := []struct {
		name       string
		a          account
		wantErr    string
		wantPasswd string
		wantGroup  string
	}{
		{"reuses gid 20", agent, "", basePasswd + "doug:x:501:20::/Users/doug:/bin/bash\n", baseGroup},
		{"new group", account{proxyName, proxyName, 970, 970, "/nonexistent", "/usr/sbin/nologin"}, "",
			basePasswd + "paddock-proxy:x:970:970::/nonexistent:/usr/sbin/nologin\n", baseGroup + "paddock-proxy:x:970:\n"},
		{"existing user kept", account{"node", "node", 1000, 1000, "/home/node", agentShell}, "", basePasswd, baseGroup},
		{"existing user other home", account{"node", "node", 1000, 1000, "/x", agentShell}, `user "node" already exists`, basePasswd, baseGroup},
		{"existing user other gid", account{"node", "node", 1000, 20, "/home/node", agentShell}, `user "node" already exists`, basePasswd, baseGroup},
		{"existing user other shell", account{"node", "node", 1000, 1000, "/home/node", "/bin/zsh"}, `user "node" already exists`, basePasswd, baseGroup},
		{"uid taken", account{"doug", "doug", 1000, 20, "/Users/doug", agentShell}, `uid 1000 is taken by user "node"`, basePasswd, baseGroup},
		{"name taken", account{"node", "node", 501, 20, "/Users/node", agentShell}, `user "node" exists with uid 1000`, basePasswd, baseGroup},
		{"group name taken", account{"doug", "node", 501, 77, "/Users/doug", agentShell}, `group "node" exists with gid 1000`, basePasswd, baseGroup},
		{"colon in home", account{"doug", "doug", 501, 20, "/a:b", agentShell}, "invalid passwd field", basePasswd, baseGroup},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "etc/passwd"), basePasswd, 0o644)
			writeFile(t, filepath.Join(root, "etc/group"), baseGroup, 0o644)
			err := ensureAccount(root, tt.a)
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if got := readFile(t, filepath.Join(root, "etc/passwd")); got != tt.wantPasswd {
				t.Errorf("passwd:\n%s\nwant:\n%s", got, tt.wantPasswd)
			}
			if got := readFile(t, filepath.Join(root, "etc/group")); got != tt.wantGroup {
				t.Errorf("group:\n%s\nwant:\n%s", got, tt.wantGroup)
			}
			if err == nil {
				if err := ensureAccount(root, tt.a); err != nil {
					t.Fatalf("second run: %v", err)
				}
				if got := readFile(t, filepath.Join(root, "etc/passwd")); got != tt.wantPasswd {
					t.Errorf("second run changed passwd:\n%s", got)
				}
			}
		})
	}
}

const sentinel = "s3cr3t-SENTINEL"

func TestParseSecrets(t *testing.T) {
	got, err := parseSecrets([]byte("A=1\r\n\nTOKEN=x=y\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A=1", "TOKEN=x=y"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, bad := range []string{
		sentinel + "\n",
		"A=1\n" + sentinel + " x=" + sentinel + "\n",
		"1A=" + sentinel + "\n",
		"A=" + sentinel + "\x00\n",
		"A=1\nA=" + sentinel + "\n",
	} {
		_, err := parseSecrets([]byte(bad))
		if err == nil {
			t.Errorf("%q: no error", bad)
			continue
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("error leaks the value: %v", err)
		}
	}
}

func TestTakeSecretsUnlinks(t *testing.T) {
	for _, data := range []string{"A=" + sentinel + "\n", "broken " + sentinel + "\n"} {
		p := filepath.Join(t.TempDir(), "secrets.env")
		writeFile(t, p, data, 0o600)
		_, err := takeSecrets(p)
		if err != nil && strings.Contains(err.Error(), sentinel) {
			t.Errorf("error leaks the value: %v", err)
		}
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("%q: secrets file still exists (err %v)", data, err)
		}
	}
	if _, err := takeSecrets(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing secrets file: no error")
	}
}

func TestAgentEnv(t *testing.T) {
	u := initcfg.User{Name: "doug", UID: 501, GID: 20, Home: "/Users/doug"}
	fixed := []string{"HOME=/Users/doug", "USER=doug", "LOGNAME=doug", "SHELL=/bin/bash"}
	tests := []struct {
		name      string
		inherited []string
		extra     []string
		secrets   []string
		want      []string
		wantErr   bool
	}{
		{
			name:      "inherited var kept",
			inherited: []string{"LANG=C.UTF-8"},
			want:      append([]string{"LANG=C.UTF-8", "PATH=" + DefaultPath}, fixed...),
		},
		{
			name:      "inherited PATH kept",
			inherited: []string{"PATH=/inherited/bin"},
			want:      append([]string{"PATH=/inherited/bin"}, fixed...),
		},
		{
			name:      "fixed HOME overrides inherited HOME",
			inherited: []string{"HOME=/elsewhere", "LANG=C"},
			want:      append([]string{"HOME=/Users/doug", "LANG=C", "PATH=" + DefaultPath}, fixed[1:]...),
		},
		{
			name:      "Config.Env and secrets override",
			inherited: []string{"TERM=dumb", "LANG=C"},
			extra:     []string{"TERM=xterm", "PATH=/opt/bin"},
			secrets:   []string{"LANG=secret", "TOKEN=t"},
			want:      append([]string{"TERM=xterm", "LANG=secret", "PATH=/opt/bin"}, append(fixed, "TOKEN=t")...),
		},
		{
			name:    "entry without '=' rejected",
			extra:   []string{"NOEQUALS"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := agentEnv(tt.inherited, u, tt.extra, tt.secrets)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestPrepareHome(t *testing.T) {
	root := t.TempDir()
	home := "/Users/doug"
	transcript := home + "/.claude/projects/-ws"
	// The engine's mount created the transcript dir and its parents.
	writeFile(t, filepath.Join(root, transcript, "old.jsonl"), "keep", 0o644)
	writeFile(t, filepath.Join(root, initcfg.SeedDir, "remote-settings.json"), `{"seed":1}`, 0o644)
	cfg := &initcfg.Config{
		User: initcfg.User{Name: "doug", UID: 501, GID: 20, Home: home},
		Claude: initcfg.ClaudeHome{
			ManagedSettings: json.RawMessage(`{"permissions":{}}`),
			ClaudeJSON:      json.RawMessage(`{"hasCompletedOnboarding":true}`),
			SeedFiles:       []string{"remote-settings.json"},
			Skills:          "/Users/doug/.botfiles/skills",
			TranscriptDir:   transcript,
		},
	}
	var chowned []string
	h := newHomeFS(root, cfg)
	h.chown = func(p string, uid, gid int) error {
		if uid != 501 || gid != 20 {
			t.Errorf("chown %s to %d:%d", p, uid, gid)
		}
		chowned = append(chowned, strings.TrimPrefix(p, root))
		return nil
	}
	if err := prepareHome(h, cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{home, home + "/.claude", home + "/.claude/projects", home + "/.claude/remote-settings.json",
		home + "/.claude.json", home + "/.claude/skills"}
	slices.Sort(chowned)
	slices.Sort(want)
	if !reflect.DeepEqual(chowned, want) {
		t.Errorf("chowned %q\nwant    %q", chowned, want)
	}
	if got := readFile(t, filepath.Join(root, transcript, "old.jsonl")); got != "keep" {
		t.Errorf("mounted dir content changed: %q", got)
	}
	seed := filepath.Join(root, home, ".claude/remote-settings.json")
	if st, err := os.Stat(seed); err != nil || st.Mode().Perm() != 0o600 || readFile(t, seed) != `{"seed":1}` {
		t.Errorf("seed file: %v %v", st, err)
	}
	if target, err := os.Readlink(filepath.Join(root, home, ".claude/skills")); err != nil || target != cfg.Claude.Skills {
		t.Errorf("skills link %q, %v", target, err)
	}
	ms := filepath.Join(root, initcfg.ManagedSettingsPath)
	if st, err := os.Stat(ms); err != nil || st.Mode().Perm() != 0o644 || readFile(t, ms) != `{"permissions":{}}` {
		t.Errorf("managed settings: %v %v", st, err)
	}

	// A second run keeps an existing ~/.claude.json and replaces the link.
	writeFile(t, filepath.Join(root, home, ".claude.json"), "mine", 0o600)
	if err := prepareHome(h, cfg); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(root, home, ".claude.json")); got != "mine" {
		t.Errorf(".claude.json overwritten: %q", got)
	}

	// A directory where the symlink goes is never replaced.
	cfg.Claude.Agents = "/Users/doug/.botfiles/agents"
	writeFile(t, filepath.Join(root, home, ".claude/agents/x"), "", 0o644)
	if err := prepareHome(h, cfg); err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Errorf("agents dir: err = %v", err)
	}
}

func TestLookPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "tool"), "", 0o644)
	writeFile(t, filepath.Join(dir, "b", "tool"), "", 0o755)
	got, err := lookPath("tool", filepath.Join(dir, "a")+":"+filepath.Join(dir, "b"))
	if err != nil || got != filepath.Join(dir, "b", "tool") {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := lookPath("missing", dir); err == nil {
		t.Error("missing: no error")
	}
}

func TestPrepareHomeLeavesFileMountAlone(t *testing.T) {
	root := t.TempDir()
	home := "/Users/doug"
	settings := home + "/.claude/settings.json"
	writeFile(t, filepath.Join(root, settings), `{"mounted":true}`, 0o644)
	cfg := &initcfg.Config{
		User:   initcfg.User{Name: "doug", UID: 501, GID: 20, Home: home},
		Mounts: []initcfg.Mount{{Path: settings, ReadOnly: true}},
	}
	h := newHomeFS(root, cfg)
	var chowned []string
	h.chown = func(p string, _, _ int) error {
		chowned = append(chowned, strings.TrimPrefix(p, root))
		return nil
	}
	if err := prepareHome(h, cfg); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(root, settings)); got != `{"mounted":true}` {
		t.Errorf("mounted file changed: %q", got)
	}
	if want := []string{home, home + "/.claude"}; !reflect.DeepEqual(chowned, want) {
		t.Errorf("chowned %q, want %q", chowned, want)
	}
	if !h.isMount(settings) {
		t.Error("file mount not recognised")
	}
}

func TestPrepareHomeRefusesRoot(t *testing.T) {
	root := t.TempDir()
	cfg := &initcfg.Config{User: initcfg.User{Name: "doug", UID: 501, GID: 20, Home: "/"}}
	h := newHomeFS(root, cfg)
	h.chown = func(p string, _, _ int) error {
		t.Errorf("chown %s", p)
		return nil
	}
	if err := prepareHome(h, cfg); err == nil || !strings.Contains(err.Error(), "filesystem root") {
		t.Errorf("prepareHome with HOME=/: err = %v", err)
	}
}
