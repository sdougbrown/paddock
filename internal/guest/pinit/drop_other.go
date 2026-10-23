//go:build !linux

package pinit

import "errors"

func dropAndExec(uid, gid int, workdir, path string, argv, env []string) error {
	return errors.New("paddock-init runs only in a linux guest")
}
