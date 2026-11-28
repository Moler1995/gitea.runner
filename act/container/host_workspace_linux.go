// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var safePathName = regexp.MustCompile(`^[\w.-]*[\w-][\w.-]*$`) // not only dots

// UseWorkspace runs the job at root/<owner>/<repo>/<repo>, its parent showing a directory of the job's own to its processes, and to the host while no other job does.
func (e *HostEnvironment) UseWorkspace(root, repository string) error {
	owner, repo, ok := strings.Cut(repository, "/")
	if !ok || !safePathName.MatchString(owner) || !safePathName.MatchString(repo) {
		return fmt.Errorf("repository %q is unsafe in a path", repository)
	}
	target, source := filepath.Join(root, owner, repo), filepath.Join(e.Root, "work")
	if err := os.MkdirAll(filepath.Join(source, repo), 0o777); err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o777); err != nil {
		return err
	}
	releaseRoot, err := privateMount(root)
	if err != nil {
		return err
	}
	thread, err := startJobThread(source, target)
	if err != nil {
		releaseRoot()
		return err
	}
	releaseView := shareView(target+".lock", source, target)
	e.Path, e.thread, e.mountTarget, e.mountSource = filepath.Join(target, repo), thread, target, source
	e.undo = func() {
		close(thread.stopped)
		releaseView()
		releaseRoot()
	}
	return nil
}

var privateMounts sync.Mutex // threads would each bind dir

// privateMount keeps dir a mount that propagates nowhere while a job of any runner uses it, so binds below it stay in the namespace that makes them.
func privateMount(dir string) (release func(), err error) {
	lock, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDONLY, 0o666)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_SH); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	privateMounts.Lock()
	defer privateMounts.Unlock()
	if unix.Mount("", dir, "", unix.MS_PRIVATE, "") != nil {
		if err := unix.Mount(dir, dir, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return nil, errors.Join(err, lock.Close())
		}
		if err := unix.Mount("", dir, "", unix.MS_PRIVATE, ""); err != nil {
			return nil, errors.Join(err, unix.Unmount(dir, unix.MNT_DETACH), lock.Close())
		}
	}
	return func() {
		if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil {
			_ = unix.Unmount(dir, unix.MNT_DETACH)
		}
		lock.Close()
	}, nil
}

// startJobThread starts a locked thread, ending with its goroutine, whose children see source at target and get no ambient CAP_SYS_ADMIN.
func startJobThread(source, target string) (*jobThread, error) {
	thread := &jobThread{calls: make(chan func()), stopped: make(chan struct{})}
	started := make(chan error)
	go func() {
		runtime.LockOSThread()
		err := unix.Unshare(unix.CLONE_NEWNS)
		if err == nil {
			unmountAll(target)
			err = unix.Mount(source, target, "", unix.MS_BIND, "")
		}
		_ = unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_LOWER, unix.CAP_SYS_ADMIN, 0, 0)
		started <- err
		if err != nil {
			return
		}
		for {
			select {
			case call := <-thread.calls:
				call()
			case <-thread.stopped:
				return
			}
		}
	}()
	return thread, <-started
}

// shareView binds source at target on the host whenever no other job holds lock, trying at once and then every second, until release.
func shareView(lock, source, target string) (release func()) {
	file, err := os.OpenFile(lock, os.O_CREATE|os.O_RDONLY, 0o666)
	if err != nil {
		return func() {}
	}
	bind := func() bool {
		if unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
			return false
		}
		unmountAll(target)
		if unix.Mount(source, target, "", unix.MS_BIND, "") != nil {
			_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
			return false
		}
		return true
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	bound := bind()
	go func() {
		defer close(stopped)
		defer file.Close()
		for !bound {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
				bound = bind()
			}
		}
		<-stop
		_ = unix.Unmount(target, unix.MNT_DETACH)
	}()
	return func() {
		close(stop)
		<-stopped
	}
}

// unmountAll drops the binds at target, copies of other jobs' views or ones a crashed runner left.
func unmountAll(target string) {
	for unix.Unmount(target, unix.MNT_DETACH) == nil {
	}
}
