package verify

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// LocalOptions configures LocalChecks.
type LocalOptions struct {
	// UID is the uid the checks must run as.
	UID int
	// StatusPath is read for the credential check; default /proc/self/status.
	StatusPath string
	// ReadOnly lists mount points that must refuse writes with EROFS.
	ReadOnly []string
	// Writable lists directories in which a new file must be creatable.
	Writable []string
}

// LocalChecks returns the non-network checks: the process holds no
// capabilities and has no_new_privs, every read-only mount refuses a
// write, and every writable directory accepts one.
func LocalChecks(o LocalOptions) []Check {
	if o.StatusPath == "" {
		o.StatusPath = "/proc/self/status"
	}
	checks := []Check{{"privileges-dropped", func(context.Context) (string, error) {
		b, err := os.ReadFile(o.StatusPath)
		if err != nil {
			return "", err
		}
		return CheckPrivileges(b, o.UID)
	}}}
	for _, p := range o.ReadOnly {
		checks = append(checks, Check{"readonly-mount " + p, func(context.Context) (string, error) {
			return writeRefused(p)
		}})
	}
	for _, p := range o.Writable {
		checks = append(checks, Check{"writable " + p, func(context.Context) (string, error) {
			f, err := os.CreateTemp(p, ".paddock-verify-*")
			if err != nil {
				return "", err
			}
			f.Close()
			return "created and removed a file", os.Remove(f.Name())
		}})
	}
	return checks
}

// CheckPrivileges checks a /proc/<pid>/status text: every uid is uid, all
// five capability sets are empty, and NoNewPrivs is 1.
func CheckPrivileges(status []byte, uid int) (string, error) {
	fields := map[string][]string{}
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok {
			fields[k] = strings.Fields(v)
		}
	}
	var bad []string
	for _, k := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		v := fields[k]
		if n, err := strconv.ParseUint(strings.Join(v, ""), 16, 64); err != nil || len(v) != 1 || n != 0 {
			bad = append(bad, fmt.Sprintf("%s=%s", k, strings.Join(v, " ")))
		}
	}
	if nnp := fields["NoNewPrivs"]; len(nnp) != 1 || nnp[0] != "1" {
		bad = append(bad, "NoNewPrivs="+strings.Join(nnp, " "))
	}
	want := strconv.Itoa(uid)
	if ids := fields["Uid"]; len(ids) != 4 || ids[0] != want || ids[1] != want || ids[2] != want || ids[3] != want {
		bad = append(bad, "Uid="+strings.Join(ids, " "))
	}
	if len(bad) > 0 {
		return "", fmt.Errorf("not dropped: %s", strings.Join(bad, ", "))
	}
	return fmt.Sprintf("uid %s, all capability sets empty, NoNewPrivs 1", want), nil
}

// writeRefused tries to create a file in the directory p, or to open the
// file p for writing, and passes only on EROFS.
func writeRefused(p string) (string, error) {
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	target, flags := p, os.O_WRONLY
	if st.IsDir() {
		target = filepath.Join(p, fmt.Sprintf(".paddock-verify-%d", os.Getpid()))
		flags |= os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(target, flags, 0o600)
	if err == nil {
		f.Close()
		if st.IsDir() {
			os.Remove(target)
		}
		return "write to " + p, errUnexpectedSuccess
	}
	if errors.Is(err, syscall.EROFS) {
		return "write refused: read-only file system", nil
	}
	return "write to " + p, fmt.Errorf("refused, but not with EROFS: %w", err)
}

// writeDenied passes when the file p refuses to open for writing and every
// directory from replaceDirs refuses a new file, so p can be neither edited
// nor replaced or redirected by a rename. A refusal for permission counts
// only when the caller does not own the file or directory, since an owner
// can chmod it writable again. It never truncates p.
func writeDenied(p string) (string, error) {
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err == nil {
		f.Close()
		return "open " + p + " for writing", errUnexpectedSuccess
	}
	if !denied(err) {
		return "open " + p + " for writing", fmt.Errorf("refused, but not for permission: %w", err)
	}
	if err := notOwned(p, err); err != nil {
		return "open " + p + " for writing", err
	}
	dirs, derr := replaceDirs(p)
	if derr != nil {
		return "", derr
	}
	details := []string{fmt.Sprintf("open for writing: %v", unwrapPath(err))}
	for _, dir := range dirs {
		probe := filepath.Join(dir, fmt.Sprintf(".paddock-verify-%d", os.Getpid()))
		d, derr := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if derr == nil {
			d.Close()
			os.Remove(probe)
			return "create a file in " + dir, errUnexpectedSuccess
		}
		if !denied(derr) {
			return "create a file in " + dir, fmt.Errorf("refused, but not for permission: %w", derr)
		}
		if err := notOwned(dir, derr); err != nil {
			return "create a file in " + dir, err
		}
		details = append(details, fmt.Sprintf("create in %s: %v", dir, unwrapPath(derr)))
	}
	return strings.Join(details, "; "), nil
}

// notOwned returns an error when refused, a write refusal for p, came from
// permissions that the caller, as p's owner, could change. A read-only
// file system refuses its owner too.
func notOwned(p string, refused error) error {
	if errors.Is(refused, syscall.EROFS) {
		return nil
	}
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if uid := st.Sys().(*syscall.Stat_t).Uid; int(uid) == os.Geteuid() {
		return fmt.Errorf("refused, but %s is owned by uid %d, which can chmod it", p, uid)
	}
	return nil
}

func denied(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EROFS)
}

func unwrapPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// ClosedNetworkChecks returns the checks for policy "none": the firewall
// cannot be flushed, no traffic leaves the guest, and no name resolves.
func ClosedNetworkChecks(o NetworkOptions) []Check {
	o = o.withDefaults()
	checks := []Check{
		{"nft-flush-refused", o.nftFlushRefused},
		{"egress-refused", func(ctx context.Context) (string, error) {
			return o.connectRefused(ctx, net.JoinHostPort(o.RawIPTarget, "443"))
		}},
		{"ipv6-refused", o.ipv6Refused},
		{"external-dns-refused", o.externalDNSRefused},
	}
	return append(checks, o.dnsChecks()...)
}

// replaceDirs returns the directories in which a new entry would replace
// p or redirect it: its own and, when p is a symlink, the directory of each
// link in the chain and of the file it ends at.
func replaceDirs(p string) ([]string, error) {
	dirs := []string{filepath.Dir(p)}
	seen := map[string]bool{}
	for hop := 0; ; hop++ {
		dir, err := filepath.EvalSymlinks(filepath.Dir(p))
		if err != nil {
			return nil, err
		}
		if !seen[dir] {
			seen[dir] = true
			if hop > 0 {
				dirs = append(dirs, dir)
			}
		}
		st, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if st.Mode()&os.ModeSymlink == 0 {
			return dirs, nil
		}
		if hop == 40 {
			return nil, fmt.Errorf("%s: too many levels of symbolic links", p)
		}
		target, err := os.Readlink(p)
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		p = target
	}
}
