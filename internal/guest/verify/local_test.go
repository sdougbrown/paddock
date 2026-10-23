package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const droppedStatus = `Name:	sh
Uid:	501	501	501	501
NoNewPrivs:	1
CapInh:	0000000000000000
CapPrm:	0000000000000000
CapEff:	0000000000000000
CapBnd:	0000000000000000
CapAmb:	0000000000000000
`

func TestCheckPrivileges(t *testing.T) {
	tests := []struct {
		name, status string
		uid          int
		want         string
	}{
		{"dropped", droppedStatus, 501, ""},
		{"bounding set kept", strings.Replace(droppedStatus, "CapBnd:	0000000000000000", "CapBnd:	00000000a80435fb", 1), 501, "CapBnd=00000000a80435fb"},
		{"no_new_privs unset", strings.Replace(droppedStatus, "NoNewPrivs:	1", "NoNewPrivs:	0", 1), 501, "NoNewPrivs=0"},
		{"missing ambient", strings.Replace(droppedStatus, "CapAmb:	0000000000000000\n", "", 1), 501, "CapAmb="},
		{"wrong uid", droppedStatus, 1000, "Uid=501 501 501 501"},
		{"effective uid 0", strings.Replace(droppedStatus, "Uid:	501	501	501	501", "Uid:	501	0	501	501", 1), 501, "Uid=501 0 501 501"},
		{"saved uid 0", strings.Replace(droppedStatus, "Uid:	501	501	501	501", "Uid:	501	501	0	501", 1), 501, "Uid=501 501 0 501"},
		{"filesystem uid 0", strings.Replace(droppedStatus, "Uid:	501	501	501	501", "Uid:	501	501	501	0", 1), 501, "Uid=501 501 501 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CheckPrivileges([]byte(tt.status), tt.uid)
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLocalChecks(t *testing.T) {
	dir := t.TempDir()
	status := filepath.Join(dir, "status")
	if err := os.WriteFile(status, []byte(droppedStatus), 0o600); err != nil {
		t.Fatal(err)
	}
	// A writable directory listed as read-only must fail its check.
	results := Run(context.Background(), LocalChecks(LocalOptions{
		UID:        501,
		StatusPath: status,
		ReadOnly:   []string{dir},
		Writable:   []string{dir},
	}))
	want := map[string]bool{"privileges-dropped": true, "readonly-mount " + dir: false, "writable " + dir: true}
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d", len(results), len(want))
	}
	for _, r := range results {
		if r.Pass != want[r.Name] {
			t.Errorf("%s: pass = %v (%s)", r.Name, r.Pass, r.Detail)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("checks left files behind: %v", entries)
	}
}

func TestWriteRefusedNotEROFS(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	results := Run(context.Background(), LocalChecks(LocalOptions{
		UID:        501,
		StatusPath: filepath.Join(dir, "status"),
		ReadOnly:   []string{dir},
	}))
	for _, r := range results {
		if r.Name == "readonly-mount "+dir {
			if r.Pass {
				t.Fatalf("permission-denied directory passed as read-only mount: %s", r.Detail)
			}
			if !strings.Contains(r.Detail, "refused, but not with EROFS") {
				t.Errorf("detail = %q; want the not-EROFS failure", r.Detail)
			}
			return
		}
	}
	t.Fatal("no readonly-mount result")
}
