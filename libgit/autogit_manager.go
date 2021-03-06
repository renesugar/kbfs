// Copyright 2017 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libgit

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"time"

	"github.com/eapache/channels"
	"github.com/keybase/client/go/logger"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/kbfs/kbfssync"
	"github.com/keybase/kbfs/libfs"
	"github.com/keybase/kbfs/libkbfs"
	billy "gopkg.in/src-d/go-billy.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

type resetReq struct {
	srcTLF     *libkbfs.TlfHandle
	srcRepo    string
	branchName string
	dstTLF     *libkbfs.TlfHandle
	dstDir     string
	doneCh     chan struct{}
}

func (r resetReq) id() string {
	return path.Join(r.dstTLF.GetCanonicalPath(), r.dstDir, r.srcRepo)
}

type deleteReq struct {
	dstTLF     *libkbfs.TlfHandle
	dstDir     string
	repo       string
	branchName string
	doneCh     chan struct{}
}

const (
	cloneFileName = "CLONING"

	// Debug tag ID for an individual autogit operation
	ctxOpID = "AGM"

	workTimeLimit = 1 * time.Hour
)

type ctxTagKey int

const (
	ctxIDKey ctxTagKey = iota
)

func autogitLockName(srcRepo string) string {
	return fmt.Sprintf(".autogit_%s.lock", srcRepo)
}

func autogitWorkingName(srcRepo string) string {
	return fmt.Sprintf(".autogit_%s.working", srcRepo)
}

func autogitLastErrName(srcRepo string) string {
	return fmt.Sprintf(".autogit_%s.lasterr", srcRepo)
}

type getNewConfigFn func(context.Context) (
	context.Context, libkbfs.Config, string, error)

// AutogitManager can clone and pull source git repos into a
// destination folder, potentially across different TLFs.  New
// requests for an operation in a destination repo are blocked by any
// ongoing requests for the same folder, and multiple outstanding
// requests for the same destination folder get rolled up into one.
type AutogitManager struct {
	config         libkbfs.Config
	kbCtx          libkbfs.Context
	kbfsInitParams *libkbfs.InitParams
	log            logger.Logger
	deferLog       logger.Logger
	resetQueue     channels.Channel
	queueDoneCh    chan struct{}
	getNewConfig   getNewConfigFn
	resetsWG       kbfssync.RepeatedWaitGroup
	updatingWG     kbfssync.RepeatedWaitGroup
	deleteQueue    channels.Channel
	deleteDoneCh   chan struct{}

	lock             sync.Mutex
	resetsInQueue    map[string]resetReq // key: resetReq.id()
	resetsInProgress map[string]resetReq // key: resetReq.id()

	registryLock           sync.RWMutex
	registeredFBs          map[libkbfs.FolderBranch]bool
	repoNodesForWatchedIDs map[libkbfs.NodeID]*repoNode
	watchedNodes           []libkbfs.Node // preventing GC on the watched nodes
	populatedRepos         map[libkbfs.NodeID]bool
}

// NewAutogitManager constructs a new AutogitManager instance, and
// launches `numWorkers` processing goroutines in the background.
func NewAutogitManager(
	config libkbfs.Config, kbCtx libkbfs.Context,
	kbfsInitParams *libkbfs.InitParams, numWorkers int) *AutogitManager {
	log := config.MakeLogger("")
	am := &AutogitManager{
		config:                 config,
		kbCtx:                  kbCtx,
		kbfsInitParams:         kbfsInitParams,
		log:                    log,
		deferLog:               log.CloneWithAddedDepth(1),
		resetQueue:             libkbfs.NewInfiniteChannelWrapper(),
		queueDoneCh:            make(chan struct{}),
		deleteQueue:            libkbfs.NewInfiniteChannelWrapper(),
		deleteDoneCh:           make(chan struct{}),
		resetsInQueue:          make(map[string]resetReq),
		resetsInProgress:       make(map[string]resetReq),
		registeredFBs:          make(map[libkbfs.FolderBranch]bool),
		repoNodesForWatchedIDs: make(map[libkbfs.NodeID]*repoNode),
		populatedRepos:         make(map[libkbfs.NodeID]bool),
	}
	am.getNewConfig = am.getNewConfigDefault
	go am.resetLoop(numWorkers)
	go am.deleteLoop()
	return am
}

// Shutdown shuts down this manager.
func (am *AutogitManager) Shutdown() {
	am.resetQueue.Close()
	am.deleteQueue.Close()
	<-am.queueDoneCh
	<-am.deleteDoneCh
}

func (am *AutogitManager) getNewConfigDefault(ctx context.Context) (
	context.Context, libkbfs.Config, string, error) {
	return getNewConfig(ctx, am.config, am.kbCtx, am.kbfsInitParams, am.log)
}

// commonTime computes the current time according to our estimate of
// the mdserver's time.  It's a very crude way of normalizing the
// local clock.
func (am *AutogitManager) commonTime(ctx context.Context) time.Time {
	offset, haveOffset := am.config.MDServer().OffsetFromServerTime()
	if !haveOffset {
		am.log.CDebugf(ctx, "No offset, cannot use common time; "+
			"falling back to local time")
		return am.config.Clock().Now()
	}
	return am.config.Clock().Now().Add(-offset)
}

func (am *AutogitManager) canWorkOnRepo(
	ctx context.Context, dstFS *libfs.FS, repo string) (
	canWork bool, err error) {
	am.log.CDebugf(ctx, "Checking if we can work on %s", repo)
	defer func() {
		am.deferLog.CDebugf(ctx, "Work check completed: canWork=%t, %+v",
			canWork, err)
	}()

	// Take the lock for the dst repo while checking the work time.
	lockFile, err := dstFS.Create(autogitLockName(repo))
	if err != nil {
		return false, err
	}
	defer func() {
		// Because we took the lock, this Close will sync/flush the
		// whole journal.
		closeErr := lockFile.Close()
		if err == nil {
			err = closeErr
		} else if closeErr != nil {
			am.log.CDebugf(ctx, "Lock close error: %+v", closeErr)
		}
	}()
	err = lockFile.Lock()
	if err != nil {
		return false, err
	}

	// See if someone else is already working on this repo by seeing
	// if the timestamp (converted to "common" time) is within the
	// expected time limit for a worker.
	workingFileName := autogitWorkingName(repo)
	fi, err := dstFS.Stat(workingFileName)
	currCommonTime := am.commonTime(ctx)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	} else if os.IsNotExist(err) {
		am.log.CDebugf(ctx, "Creating new working file for %s", repo)
		f, err := dstFS.Create(workingFileName)
		if err != nil {
			return false, err
		}
		err = f.Close()
		if err != nil {
			return false, err
		}
	} else { // err == nil
		modCommonTime := fi.ModTime()
		if modCommonTime.Add(workTimeLimit).After(currCommonTime) {
			am.log.CDebugf(ctx, "Other worker is still working on %s; "+
				"modCommonTime=%s, currCommonTime=%s, workTimeLimit=%s",
				repo, modCommonTime, currCommonTime, workTimeLimit)
			// The other worker is still running within the time
			// limit.
			return false, nil
		}
		am.log.CDebugf(ctx, "Other work expired on %s; "+
			"modCommonTime=%s, currCommonTime=%s, workTimeLimit=%s",
			repo, modCommonTime, currCommonTime, workTimeLimit)
	}

	am.log.CDebugf(ctx, "Setting work common time to %s", currCommonTime)
	err = dstFS.Chtimes(workingFileName, time.Time{}, currCommonTime)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (am *AutogitManager) workDoneOnRepo(
	ctx context.Context, dstFS *libfs.FS, repo string, workErr error) (
	err error) {
	am.log.CDebugf(ctx, "Completing work on %s, workErr=%+v", repo, workErr)
	defer func() {
		am.deferLog.CDebugf(ctx, "Work done completed: %+v", err)
	}()

	// Take the lock for the dst repo while checking the work time.
	lockFile, err := dstFS.Create(autogitLockName(repo))
	if err != nil {
		return err
	}
	defer func() {
		// Because we took the lock, this Close will sync/flush the
		// whole journal.
		closeErr := lockFile.Close()
		if err == nil {
			err = closeErr
			// TODO: if `closeErr != nil`, write it to the lasterr
			// file somehow, even though we're no longer under lock?
		} else if closeErr != nil {
			am.log.CDebugf(ctx, "Lock close error: %+v", closeErr)
		}
	}()
	err = lockFile.Lock()
	if err != nil {
		return err
	}

	err = dstFS.Remove(autogitWorkingName(repo))
	if err != nil {
		return err
	}

	// Remove the old lasterr file if it exists.  TODO: check if we
	// are the user who was supposed to be doing work?
	err = dstFS.Remove(autogitLastErrName(repo))
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	if workErr == nil {
		return nil
	}

	// Otherwise write the last error
	lastErrFile, err := dstFS.Create(autogitLastErrName(repo))
	if err != nil {
		return err
	}
	defer lastErrFile.Close()
	_, err = io.WriteString(lastErrFile, workErr.Error())
	if err != nil {
		return err
	}
	return nil
}

func (am *AutogitManager) doReset(ctx context.Context, req resetReq) (
	err error) {
	am.log.CDebugf(ctx, "Processing reset request from %s/%s to %s/%s",
		req.srcTLF.GetCanonicalPath(), req.srcRepo,
		req.dstTLF.GetCanonicalPath(), req.dstDir)
	defer func() {
		am.deferLog.CDebugf(ctx, "Reset request completed: %+v", err)
	}()

	// Make a new single-op config for processing this request.
	ctx, gitConfig, tempDir, err := am.getNewConfig(ctx)
	if err != nil {
		return err
	}
	// Cloned/pull data should be regular data blocks.
	gitConfig.SetDefaultBlockType(keybase1.BlockType_DATA)
	defer func() {
		gitConfig.Shutdown(ctx)
		rmErr := os.RemoveAll(tempDir)
		if rmErr != nil {
			am.log.CWarningf(
				ctx, "Error cleaning storage dir %s: %+v\n", tempDir, rmErr)
		}
	}()

	uniqID, err := makeUniqueID(ctx, gitConfig)
	if err != nil {
		return err
	}

	// Construct a src repo FS.
	srcRepoFS, _, err := GetRepoAndID(
		ctx, gitConfig, req.srcTLF, req.srcRepo, uniqID)
	if err != nil {
		return err
	}

	// And a dst parent checkout FS, which better already exist.
	dstFS, err := libfs.NewFS(
		ctx, gitConfig, req.dstTLF, req.dstDir, uniqID,
		keybase1.MDPriorityNormal)
	if err != nil {
		return err
	}

	canWork, err := am.canWorkOnRepo(ctx, dstFS, req.srcRepo)
	if err != nil {
		return err
	}
	if !canWork {
		am.log.CDebugf(ctx,
			"Another worker is currently in charge; skipping reset")
		// TODO: retry in a little while?
		return nil
	}
	defer func() {
		workDoneErr := am.workDoneOnRepo(ctx, dstFS, req.srcRepo, err)
		if err == nil {
			err = workDoneErr
		}
	}()

	dstRepoFS, err := dstFS.Chroot(req.srcRepo)
	if err != nil {
		return err
	}

	// For now, assume the branch name refers to a ref head.
	branch := plumbing.ReferenceName(
		fmt.Sprintf("refs/heads/%s", req.branchName))
	am.log.CDebugf(ctx, "Starting the reset")
	return Reset(ctx, srcRepoFS, dstRepoFS, branch)
}

func (am *AutogitManager) markResetReqInProgress(req resetReq) (
	waitCh <-chan struct{}) {
	am.lock.Lock()
	defer am.lock.Unlock()
	inProgress, ok := am.resetsInProgress[req.id()]
	if ok {
		return inProgress.doneCh
	}
	delete(am.resetsInQueue, req.id())
	am.resetsInProgress[req.id()] = req
	return nil
}

func (am *AutogitManager) clearFromInProgress(req resetReq) {
	am.lock.Lock()
	defer am.lock.Unlock()
	delete(am.resetsInProgress, req.id())
	am.resetsWG.Done()
}

func (am *AutogitManager) resetWorker(wg *sync.WaitGroup) {
	defer wg.Done()
	for reqInt := range am.resetQueue.Out() {
		req := reqInt.(resetReq)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx = libkbfs.CtxWithRandomIDReplayable(
			ctx, ctxIDKey, ctxOpID, am.log)
		for {
			waitCh := am.markResetReqInProgress(req)
			if waitCh == nil {
				break
			}
			// We need to wait for any in-progress resets, since the
			// File.Lock is taken per-instance, not per-goroutine.
			am.log.CDebugf(ctx, "Waiting to process reset request from "+
				"%s/%s to %s/%s",
				req.srcTLF.GetCanonicalPath(), req.srcRepo,
				req.dstTLF.GetCanonicalPath(), req.dstDir)
			<-waitCh
			am.log.CDebugf(ctx, "Done waiting")
		}

		_ = am.doReset(ctx, req)

		// We can clear from in-progress or close in any order.  If
		// there's a race in between, the only thinga affected in the
		// `for` loop above, perhaps resulting in one needless
		// iteration before the in-progress channel is closed.
		am.clearFromInProgress(req)
		close(req.doneCh)
	}
}

func (am *AutogitManager) resetLoop(numWorkers int) {
	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go am.resetWorker(&wg)
	}
	wg.Wait()
	close(am.queueDoneCh)
}

func (am *AutogitManager) queueReset(ctx context.Context, req resetReq) (
	doneCh chan struct{}, err error) {
	id := req.id()
	doneCh = func() chan struct{} {
		am.lock.Lock()
		defer am.lock.Unlock()
		if req, ok := am.resetsInQueue[id]; ok {
			return req.doneCh
		}
		am.resetsWG.Add(1)
		am.resetsInQueue[id] = req
		return nil
	}()
	if doneCh != nil {
		return doneCh, nil
	}
	select {
	case am.resetQueue.In() <- req:
		am.log.CDebugf(ctx, "Queued new reset request for %s", id)
	case <-ctx.Done():
		// We've already promised to queue this, and may have turned
		// away other requests for it already, so we better queue it.
		go func() { am.resetQueue.In() <- req }()
		return nil, ctx.Err()
	}
	return req.doneCh, nil
}

func (am *AutogitManager) removeLock(
	ctx context.Context, gitConfig libkbfs.Config, fs *libfs.FS, repo string) (
	err error) {
	err = fs.Remove(autogitLockName(repo))
	if err != nil {
		return err
	}
	err = fs.SyncAll()
	if err != nil {
		return err
	}
	jServer, err := libkbfs.GetJournalServer(gitConfig)
	if err != nil {
		return err
	}
	return jServer.FinishSingleOp(
		ctx, fs.RootNode().GetFolderBranch().Tlf, nil,
		keybase1.MDPriorityNormal)
}

func (am *AutogitManager) doDelete(req deleteReq) (err error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = libkbfs.CtxWithRandomIDReplayable(
		ctx, ctxIDKey, ctxOpID, am.log)

	am.log.CDebugf(ctx, "Processing delete request of %s/%s/%s",
		req.dstTLF.GetCanonicalPath(), req.dstDir, req.repo)
	defer func() {
		am.deferLog.CDebugf(ctx, "Delete request completed: %+v", err)
	}()

	// Make a new single-op config for processing this request.
	ctx, gitConfig, tempDir, err := am.getNewConfig(ctx)
	if err != nil {
		return err
	}
	// Autogit data should be regular data blocks.
	gitConfig.SetDefaultBlockType(keybase1.BlockType_DATA)
	defer func() {
		gitConfig.Shutdown(ctx)
		rmErr := os.RemoveAll(tempDir)
		if rmErr != nil {
			am.log.CWarningf(
				ctx, "Error cleaning storage dir %s: %+v\n", tempDir, rmErr)
		}
	}()

	uniqID, err := makeUniqueID(ctx, gitConfig)
	if err != nil {
		return err
	}

	dstFS, err := libfs.NewFS(
		ctx, gitConfig, req.dstTLF, req.dstDir, uniqID,
		keybase1.MDPriorityNormal)
	if err != nil {
		return err
	}

	canWork, err := am.canWorkOnRepo(ctx, dstFS, req.repo)
	if err != nil {
		return err
	}
	if !canWork {
		am.log.CDebugf(ctx,
			"Another worker is currently in charge; skipping delete")
		// TODO: retry in a little while?
		return nil
	}
	defer func() {
		workDoneErr := am.workDoneOnRepo(ctx, dstFS, req.repo, err)
		if err == nil {
			err = workDoneErr
		}
		// Remove the lock file.  This happens outside of the main
		// deletion single-op, and so won't appear strictly atomically
		// with the rest of the delete.
		rmErr := am.removeLock(ctx, gitConfig, dstFS, req.repo)
		if err == nil {
			err = rmErr
		}
	}()

	fi, err := dstFS.Stat(req.repo)
	if err != nil {
		return err
	}
	return recursiveDelete(ctx, dstFS, fi)
}

func (am *AutogitManager) deleteLoop() {
	for reqInt := range am.deleteQueue.Out() {
		req := reqInt.(deleteReq)
		_ = am.doDelete(req)
		close(req.doneCh)
	}
	close(am.deleteDoneCh)
}

func (am *AutogitManager) makeCloningFile(
	ctx context.Context, dstRepoFS billy.Filesystem, srcTLF *libkbfs.TlfHandle,
	srcRepo, branchName string) error {
	am.log.CDebugf(ctx, "Making CLONING file")
	cloneFile, err := dstRepoFS.Create(cloneFileName)
	if err != nil {
		return err
	}
	defer cloneFile.Close()
	_, err = io.WriteString(cloneFile,
		fmt.Sprintf("%s/%s:%s", srcTLF.GetCanonicalPath(), srcRepo, branchName))
	if err != nil {
		return err
	}
	return nil
}

// Clone queues a request to clone the `branchName` branch of the
// `srcRepo` repo from the TLF `srcTLF`, into a subdirectory named
// `dstDir/srcRepo` in the TLF `dstTLF`. `dstDir` must already exist
// in `dstTLF`.
//
// It returns a channel that, when closed, indicates the clone request
// has finished (though not necessarily successfully).  The caller may
// have to sync from the server to ensure they are see the changes,
// however.
//
// If the cloned directory doesn't exist yet, this function creates a
// placeholder "CLONING" file to let users know the clone is happening
// in the background.
//
// Note that if there's already data in `dstDir/srcRepo`, this
// tramples it, so the caller must ensure that they are requesting a
// clone from the same repo/branch as the existing repo that might
// have already been there.
//
// If the caller specifies a non-master `branchName`, they should make
// sure `dstDir` is unique for that branch; i.e., the branch name
// should appear in the path somewhere.
func (am *AutogitManager) Clone(
	ctx context.Context, srcTLF *libkbfs.TlfHandle, srcRepo, branchName string,
	dstTLF *libkbfs.TlfHandle, dstDir string) (
	doneCh <-chan struct{}, err error) {
	am.log.CDebugf(ctx, "Autogit clone request from %s/%s:%s to %s/%s",
		srcTLF.GetCanonicalPath(), srcRepo, branchName,
		dstTLF.GetCanonicalPath(), dstDir)
	defer func() {
		am.deferLog.CDebugf(ctx, "Clone request processed: %+v", err)
	}()

	dstFS, err := libfs.NewFS(
		ctx, am.config, dstTLF, dstDir, "", keybase1.MDPriorityNormal)
	if err != nil {
		return nil, err
	}

	// Take dst lock and create "CLONING" file if needed.
	lockFile, err := dstFS.Create(autogitLockName(srcRepo))
	if err != nil {
		return nil, err
	}
	defer func() {
		closeErr := lockFile.Close()
		if err == nil {
			err = closeErr
		}
	}()
	err = lockFile.Lock()
	if err != nil {
		return nil, err
	}

	err = dstFS.MkdirAll(srcRepo, 0600)
	if err != nil {
		return nil, err
	}

	dstRepoFS, err := dstFS.Chroot(srcRepo)
	if err != nil {
		return nil, err
	}

	fis, err := dstRepoFS.ReadDir("")
	if err != nil {
		return nil, err
	}
	if len(fis) == 0 {
		err = am.makeCloningFile(ctx, dstRepoFS, srcTLF, srcRepo, branchName)
		if err != nil {
			return nil, err
		}
		// Sync the CLONING file before starting the reset.
		err = lockFile.Unlock()
		if err != nil {
			return nil, err
		}
	}

	req := resetReq{
		srcTLF, srcRepo, branchName, dstTLF, dstDir, make(chan struct{}),
	}
	return am.queueReset(ctx, req)
}

// Pull queues a request to pull the `branchName` branch of the
// `srcRepo` repo from the TLF `srcTLF`, into a subdirectory named
// `dstDir/srcRepo` in the TLF `dstTLF`. `dstDir/srcRepo` must already
// exist in `dstTLF`.
//
// It returns a channel that, when closed, indicates the pull request
// has finished (though not necessarily successfully).  The caller may
// have to sync from the server to ensure they are see the changes,
// however.
//
// Note that this tramples any data that was previously in
// `dstDir/srcRepo`, so the caller must ensure that they are
// requesting a pull from the correct repo/branch as the existing repo
// that was already there.
//
// If the caller specifies a non-master `branchName`, they should make
// sure `dstDir` is unique for that branch; i.e., the branch name
// should appear in the path somewhere.
func (am *AutogitManager) Pull(
	ctx context.Context, srcTLF *libkbfs.TlfHandle, srcRepo, branchName string,
	dstTLF *libkbfs.TlfHandle, dstDir string) (
	doneCh <-chan struct{}, err error) {
	am.log.CDebugf(ctx, "Autogit pull request from %s/%s:%s to %s/%s",
		srcTLF.GetCanonicalPath(), srcRepo, branchName,
		dstTLF.GetCanonicalPath(), dstDir)
	defer func() {
		am.deferLog.CDebugf(ctx, "Pull request processed: %+v", err)
	}()

	req := resetReq{
		srcTLF, srcRepo, branchName, dstTLF, dstDir, make(chan struct{}),
	}
	return am.queueReset(ctx, req)
}

// Delete queues a request to delete an autogit destination subdir
// named `dstDir/srcRepo` in the TLF `dstTLF`.
//
// It returns a channel that, when closed, indicates the pull request
// has finished (though not necessarily successfully).  The caller may
// have to sync from the server to ensure they are see the changes,
// however.
func (am *AutogitManager) Delete(
	ctx context.Context, dstTLF *libkbfs.TlfHandle, dstDir string,
	repo, branchName string) (doneCh <-chan struct{}, err error) {
	am.log.CDebugf(ctx, "Autogit delete request for %s/%s:%s",
		dstTLF.GetCanonicalPath(), dstDir, repo, branchName)
	defer func() {
		am.deferLog.CDebugf(ctx, "Delete request processed: %+v", err)
	}()

	req := deleteReq{
		dstTLF, dstDir, repo, branchName, make(chan struct{}),
	}

	select {
	case am.deleteQueue.In() <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return req.doneCh, nil
}

func (am *AutogitManager) registerRepoNode(
	nodeToWatch libkbfs.Node, rn *repoNode) {
	am.registryLock.Lock()
	defer am.registryLock.Unlock()
	am.repoNodesForWatchedIDs[nodeToWatch.GetID()] = rn
	am.watchedNodes = append(am.watchedNodes, nodeToWatch)
	fb := nodeToWatch.GetFolderBranch()
	if !am.registeredFBs[fb] {
		err := am.config.Notifier().RegisterForChanges(
			[]libkbfs.FolderBranch{fb}, am)
		if err != nil {
			am.log.CWarningf(nil, "Error registering %s: +%v", fb.Tlf, err)
			return
		}
		am.registeredFBs[fb] = true
	}
}

func (am *AutogitManager) isRepoNodePopulated(rn *repoNode) bool {
	am.registryLock.RLock()
	defer am.registryLock.RUnlock()
	return am.populatedRepos[rn.GetID()]
}

func (am *AutogitManager) populateDone(rn *repoNode) {
	am.registryLock.Lock()
	defer am.registryLock.Unlock()
	am.populatedRepos[rn.GetID()] = true
}

// LocalChange implements the libkbfs.Observer interface for AutogitManager.
func (am *AutogitManager) LocalChange(
	ctx context.Context, node libkbfs.Node, wr libkbfs.WriteRange) {
	// Do nothing.
}

func (am *AutogitManager) notifyNodeLocked(
	ctx context.Context, id libkbfs.NodeID) {
	rn, ok := am.repoNodesForWatchedIDs[id]
	if !ok {
		return
	}

	am.updatingWG.Add(1)
	go func() {
		defer am.updatingWG.Done()
		ctx := libkbfs.BackgroundContextWithCancellationDelayer()
		ctx = libkbfs.CtxWithRandomIDReplayable(
			ctx, ctxIDKey, ctxOpID, am.log)
		rn.updated(ctx)
	}()
}

// BatchChanges implements the libkbfs.Observer interface for AutogitManager.
func (am *AutogitManager) BatchChanges(
	ctx context.Context, _ []libkbfs.NodeChange,
	affectedNodeIDs []libkbfs.NodeID) {
	am.registryLock.RLock()
	defer am.registryLock.RUnlock()
	for _, id := range affectedNodeIDs {
		am.notifyNodeLocked(ctx, id)
	}
}

// TlfHandleChange implements the libkbfs.Observer interface for
// AutogitManager.
func (am *AutogitManager) TlfHandleChange(
	ctx context.Context, newHandle *libkbfs.TlfHandle) {
	// Do nothing.
}

// StartAutogit launches autogit, and returns a function that should
// be called on shutdown.
func StartAutogit(kbCtx libkbfs.Context, config libkbfs.Config,
	kbfsInitParams *libkbfs.InitParams, numWorkers int) func() {
	am := NewAutogitManager(config, kbCtx, kbfsInitParams, numWorkers)
	rw := rootWrapper{am}
	config.AddRootNodeWrapper(rw.wrap)
	return am.Shutdown
}
