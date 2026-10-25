package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// The inode is never replaced or removed: otherwise two agents could lock different files.
func lockHost(runtimeDir, stateDir string) (*os.File, error) {
	info, err := os.Lstat(runtimeDir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ok || int(stat.Uid) != os.Geteuid() {
		return nil, errors.New("worker runtime directory must be private and owned by this user")
	}
	file, err := os.OpenFile(filepath.Join(runtimeDir, "ordo-worker.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) { return nil, errors.Join(err, file.Close()) }
	info, err = file.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fail(errors.New("worker ownership file must be private and regular"))
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("worker host/user already has an active agent: %w", err))
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return fail(err)
	}
	if len(data) != 0 {
		var owner string
		if err := json.Unmarshal(data, &owner); err != nil || owner == "" {
			return fail(errors.New("invalid worker ownership record; reconcile before repairing it"))
		}
		if owner != stateDir {
			return fail(fmt.Errorf("worker host/user is bound to %s; reconcile unfinished attempts there before changing state directories", owner))
		}
	}
	return file, nil
}

func bindHost(file *os.File, stateDir string) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	// Rewriting an existing binding could leave it empty if the agent crashes mid-write.
	if info.Size() != 0 {
		return nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(stateDir); err != nil {
		return err
	}
	return file.Sync()
}

func releaseHost(file *os.File, clear bool) error {
	if file == nil {
		return nil
	}
	var err error
	if clear {
		err = file.Truncate(0)
		if err == nil {
			err = file.Sync()
		}
	}
	return errors.Join(err, file.Close())
}
