// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package reproxy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"team/foundry-x/re-client/internal/pkg/deps"
	"team/foundry-x/re-client/internal/pkg/inputprocessor/pathtranslator"
	"team/foundry-x/re-client/internal/pkg/logger"
	"team/foundry-x/re-client/pkg/inputprocessor"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/client"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/command"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/filemetadata"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/outerr"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/rexec"

	lpb "team/foundry-x/re-client/api/log"
	ppb "team/foundry-x/re-client/api/proxy"

	cpb "github.com/bazelbuild/remote-apis-sdks/go/api/command"

	log "github.com/golang/glog"
)

const (
	// Percentile download latency at which any action taking longer is considered an outlier that
	// should be raced with local execution.
	downloadPercentileCutoff = 90
)

type testOnlyCtxKey int

const (
	testOnlyBlockRemoteExecKey testOnlyCtxKey = iota
	testOnlyBlockLocalExecKey
	testOnlyBlockFallbackKey
)

type action struct {
	// Below parameters are set by the struct creator.
	cmd                    *command.Command
	fmc                    filemetadata.Cache
	forecast               *Forecast
	lbls                   map[string]string
	toolchainInputs        []string
	oe                     outerr.OutErr
	rec                    *logger.LogRecord
	rOpt                   *ppb.RemoteExecutionOptions
	lOpt                   *ppb.LocalExecutionOptions
	execStrategy           ppb.ExecutionStrategy_Value
	compare                bool
	numRetriesIfMismatched int
	numLocalReruns         int
	numRemoteReruns        int
	reclientTimeout        int
	cmdEnvironment         []string
	cancelFunc             func(error)
	racingBias             float64
	windowsCross           bool
	racingTmp              string

	// Below parameters are computed by struct functions.
	execContext   *rexec.Context
	parser        *deps.Parser
	dFile         string
	depsFile      string
	res           *command.Result
	rawInOutFiles []string
	digest        string
}

func (a *action) runLocal(ctx context.Context, pool *LocalPool) {
	cmd := a.cmd
	if a.lOpt.GetWrapper() != "" {
		cmd = &command.Command{}
		*cmd = *a.cmd
		cmd.Args = append([]string{a.lOpt.GetWrapper()}, a.cmd.Args...)
	}
	log.V(2).Infof("%v: Executing locally...\n%s", cmd.Identifiers.ExecutionID, strings.Join(cmd.Args, " "))
	exitCode, err := pool.Run(ctx, ctx, cmd, a.lbls, a.oe, a.rec)
	a.res = command.NewResultFromExitCode(exitCode)
	if exitCode == 0 && err != nil {
		a.res = command.NewLocalErrorResult(err)
	}
	a.rec.LocalMetadata.ExecutedLocally = true
	a.rec.LocalMetadata.Result = command.ResultToProto(a.res)
}

func (a *action) runRemote(ctx context.Context, client *rexec.Client) {
	opts := execOptionsFromProto(a.rOpt)
	excludeUnchanged := opts.DownloadOutputs && opts.PreserveUnchangedOutputMtime
	if excludeUnchanged {
		// When excluding unchanged outputs from mtime modification, we'll download only those files
		// that have changed. So we're setting DownloadOutputs option to false to prevent all outputs
		// from being downloaded by default.
		opts.DownloadOutputs = false
	}
	cmd := a.cmd
	if a.rOpt.GetWrapper() != "" {
		cmd = &command.Command{}
		*cmd = *a.cmd
		cmd.Args = append([]string{a.rOpt.GetWrapper()}, a.cmd.Args...)
	}
	var res *command.Result
	var meta *command.Metadata
	ec, err := client.NewContext(ctx, cmd, opts, a.oe)
	if err != nil {
		res, meta = command.NewLocalErrorResult(err), &command.Metadata{}
	} else if ec.GetCachedResult(); ec.Result != nil {
		res, meta = ec.Result, ec.Metadata
	} else {
		ec.ExecuteRemotely()
		res, meta = ec.Result, ec.Metadata
	}
	a.digest = meta.ActionDigest.String()
	a.rec.RemoteMetadata = logger.CommandRemoteMetadataToProto(meta)
	a.rec.RemoteMetadata.Result = command.ResultToProto(res)
	a.res = res
	if res.Err == nil && excludeUnchanged {
		outs, err := ec.GetFlattenedOutputs()
		if err != nil {
			log.Errorf("%v: Unable to get flattened outputs from Action Result: %v",
				a.cmd.Identifiers.ExecutionID, err)
			return
		}
		outs = a.excludeUnchangedOutputs(outs, a.cmd.ExecRoot)
		ec.DownloadSpecifiedOutputs(outs, a.cmd.ExecRoot)
	}
}

func (a *action) createTmpDir() (string, func(), error) {
	tmpDir := filepath.Join(a.racingTmp, a.cmd.Identifiers.ExecutionID)
	if err := os.Mkdir(tmpDir, os.ModePerm); err != nil {
		return "", nil, err
	}
	return tmpDir, func() {
		if err := removeContents(tmpDir); err != nil {
			log.Warningf("%v: could not remove temp directory for remote output: %v", a.cmd.Identifiers.ExecutionID, err)
		}
	}, nil
}

func removeContents(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		err = os.RemoveAll(filepath.Join(dir, name))
		if err != nil {
			return err
		}
	}
	return os.RemoveAll(dir)
}

// depth returns the number of segments in a file path
// Splitting by os.PathSeparator is insufficient as windows considers
// both / and \ to be valid path separators
func depth(path string) int {
	if path == "" {
		return 0
	}
	d := 1
	for i := 0; i < len(path); i++ {
		if os.IsPathSeparator(path[i]) {
			d++
		}
	}
	return d
}

// toRemoteWorkingDir returns a canonical path that has the same number of
// segments as workingDir to ensure relative paths in commands still behave
// correctly.
// This path is normalized so that all separators are os.PathSeparator
func toRemoteWorkingDir(workingDir string) string {
	if workingDir == "" || workingDir == "." {
		return workingDir
	}
	dirDepth := depth(filepath.Clean(workingDir))
	elem := make([]string, dirDepth)
	elem[0] = "set_by_reclient"
	for i := 1; i < dirDepth; i++ {
		elem[i] = "a"
	}
	return filepath.Join(elem...)
}

type resultType int

const (
	remote resultType = iota
	local
	canceled
)

type raceResult struct {
	res *command.Result
	oe  outerr.OutErr
	t   resultType
}

func (a *action) race(ctx context.Context, client *rexec.Client, pool *LocalPool, numFallbacks *int64, maxHoldoff time.Duration) {
	cCtx, cancel := context.WithCancel(ctx)
	// Get digests and mtimes of existing outs on disk if needed for comparison later
	var preExecOuts map[string]filemetadata.Metadata
	var err error
	opts := execOptionsFromProto(a.rOpt)
	if opts.PreserveUnchangedOutputMtime {
		if preExecOuts, err = a.getPreExecOutsInfo(); err != nil {
			log.Warningf("%v: could not get digests and mtimes of existing outputs on disk. Unchanged output mtimes may NOT be preserved: %v", a.cmd.Identifiers.ExecutionID, err)
		}
	}
	tmpDir, cleanup, err := a.createTmpDir()
	if err != nil {
		log.Warningf("%v: could not create temp directory for remote output: %v", a.cmd.Identifiers.ExecutionID, err)
		a.res = command.NewLocalErrorResult(err)
		return
	}
	defer func() {
		go cleanup()
	}()
	ch := make(chan raceResult, 2)
	lCh := make(chan bool)

	go func() {
		// Run remotely with a background context to ensure remote execution
		// happens to increase cacheability of future builds. We need to copy
		// the testOnlyBlock value to ensure we can still test blocking remote
		// results.
		bCtx := context.WithValue(context.Background(), testOnlyBlockRemoteExecKey, ctx.Value(testOnlyBlockRemoteExecKey))
		ch <- a.runRemoteRace(bCtx, cCtx, client, lCh, tmpDir, maxHoldoff)
	}()
	go func() {
		select {
		case <-lCh:
			ch <- a.runLocalRace(ctx, cCtx, pool)
		case <-cCtx.Done():
			ch <- raceResult{t: canceled}
		}
	}()
	var winner raceResult
	select {
	case winner = <-ch:
		// If either remote or local finished then cancel the context to abort the other.
		// Note that local is only aborted if execution was still queued and not started.
		// Once local execution starts, it cannot be aborted.
		// If remote was canceled then let local execution continue
		if winner.t != canceled {
			cancel()
			log.V(2).Infof("%v: Canceled after %v won", a.cmd.Identifiers.ExecutionID, winner.t)
		}
	case <-ctx.Done():
		a.res = command.NewLocalErrorResult(ctx.Err())
		return
	}
	if v := ctx.Value(testOnlyBlockFallbackKey); v != nil {
		v.(func())()
	}
	if winner.t == remote || winner.t == canceled {
		// In case winner is remote, wait for local to complete or cancel before continuing.
		// In case winner was canceled, wait for the real winner.
		select {
		case rr := <-ch:
			if winner.t == canceled && rr.t == local {
				atomic.AddInt64(numFallbacks, 1)
			}
			if rr.t == local {
				// Local was not canceled. Will make local the winner.
				winner = rr
			}
			// Otherwise local was canceled, which means remote should continue to be the
			// winner.
		case <-ctx.Done():
			a.res = command.NewLocalErrorResult(ctx.Err())
			return
		}
	}
	from := time.Now()
	if winner.t == remote {
		log.V(2).Infof("%v: Using remote result", a.cmd.Identifiers.ExecutionID)
		if err := a.moveOutputsFromTemp(tmpDir); err != nil {
			a.res = command.NewLocalErrorResult(err)
			return
		}
		if opts.PreserveUnchangedOutputMtime {
			if err = a.restoreUnchangedOutputMtimes(preExecOuts); err != nil {
				log.Errorf("%v: Was unable to restore mtimes for unchanged outputs: %v",
					a.cmd.Identifiers.ExecutionID, err)
			}
		}
		a.rec.RemoteMetadata = logger.CommandRemoteMetadataToProto(a.execContext.Metadata)
		a.rec.RemoteMetadata.Result = command.ResultToProto(winner.res)
		a.res = winner.res
	}
	if winner.t == local {
		log.V(2).Infof("%v: Using local result", a.cmd.Identifiers.ExecutionID)
	}
	if winner.t == canceled && winner.res != nil {
		log.V(2).Infof("%v: Both local and remote were canceled", a.cmd.Identifiers.ExecutionID)
		a.res = winner.res
	}
	a.rec.RecordEventTime(logger.EventRacingFinalizationOverhead, from)
	a.oe = winner.oe
}

// runRemoteRace runs the remote part of the race. lCh is used to start local
// execution when remote execution is expected to take time.
func (a *action) runRemoteRace(ctx, cCtx context.Context, client *rexec.Client, lCh chan<- bool, tmpDir string, maxHoldoff time.Duration) raceResult {
	opts := execOptionsFromProto(a.rOpt)
	opts.DownloadOutputs = false // We want to download them to tmpDir instead of execRoot.
	rcmd := a.cmd
	// TODO: refactor this so its commonly used in runRemote.
	if a.rOpt.GetWrapper() != "" {
		rcmd = &command.Command{}
		*rcmd = *a.cmd
		rcmd.Args = append([]string{a.rOpt.GetWrapper()}, a.cmd.Args...)
	}
	rOE := outerr.NewRecordingOutErr()
	// Use the non-cancellable context since we don't want to abort the remote execution
	// attempt even if local wins. This helps get cache hits for subsequent builds
	var err error
	if a.execContext, err = client.NewContext(ctx, rcmd, opts, rOE); err != nil {
		log.Warningf("%v: Failed to create execution context: %v", a.cmd.Identifiers.ExecutionID, err)
		close(lCh)
		return raceResult{t: canceled, res: command.NewLocalErrorResult(err)}
	}
	a.execContext.GetCachedResult()
	if a.execContext.Result == nil {
		// If action is a cache miss, start remote execution and local execution.
		log.V(2).Infof("%v: Cache miss, starting race", a.cmd.Identifiers.ExecutionID)
		close(lCh)
		a.execContext.ExecuteRemotely()
		log.V(2).Infof("%v: Executed remotely: %+v", a.cmd.Identifiers.ExecutionID, a.execContext.Result)
		select {
		case <-cCtx.Done():
			// If local has already completed, no need to download outputs.
			return raceResult{t: canceled}
		default:
		}
	} else {
		// If action is a cache hit, wait for dl milliseconds, then start local execution.
		go func() {
			dl, err := a.forecast.PercentileDownloadLatency(a, downloadPercentileCutoff)
			if err != nil {
				log.Warningf("%v: Failed to get download latency prediction: %v", a.cmd.Identifiers.ExecutionID, err)
				dl = maxHoldoff
			}
			// racingBias * 2 so that at racingBias = 0.5 (balanced), the multiplier is 1.0. Bias towards
			// speed increases the multiplier > 1, increasing the hold off period. Bias towards
			// bandwidth decreases the multiplier < 1, decreasing the hold off period.
			sl := time.Duration(float64(dl.Milliseconds())*(a.racingBias*2)) * time.Millisecond
			if sl > maxHoldoff {
				sl = maxHoldoff // Clamp holdoff to be no more than maxHoldoff
			}
			time.Sleep(sl)
			log.V(2).Infof("%v: Hold off of %v done, will signal local execution", sl, a.cmd.Identifiers.ExecutionID)
			close(lCh)
		}()
	}
	// Store action result before calling DownloadOutputs, which will overwrite the result in the
	// exec context.
	res := a.execContext.Result
	if !res.IsOk() {
		log.Warningf("%v: Remote execution failed with %+v, Waiting for local.", a.cmd.Identifiers.ExecutionID, res)
		log.Warningf("%v: stdout: %s\n stderr: %s", a.cmd.Identifiers.ExecutionID, rOE.Stdout(), rOE.Stderr())
		return raceResult{t: canceled, res: res}
	}
	log.V(2).Infof("Downloading action outputs to temp dir: %v", tmpDir)
	a.execContext.DownloadOutputs(tmpDir)
	select {
	case <-cCtx.Done():
		// If local has already completed, no need to download outputs.
		return raceResult{t: canceled}
	default:
	}
	if !a.execContext.Result.IsOk() {
		// Download failed.
		return raceResult{t: canceled, res: a.execContext.Result, oe: rOE}
	}
	if v := ctx.Value(testOnlyBlockRemoteExecKey); v != nil {
		v.(func())()
	}
	return raceResult{t: remote, res: a.execContext.Result, oe: rOE}
}

// runLocalRace runs the local portion of the race. If local execution resources
// are already acquired and local execution has started, its results will be
// used regardless of the state of remote execution. Local execution does not
// get canceled if it starts, even if remote execution is done.
func (a *action) runLocalRace(ctx, cCtx context.Context, pool *LocalPool) raceResult {
	log.V(2).Infof("%v: Running local", a.cmd.Identifiers.ExecutionID)
	lr := logger.NewLogRecord()
	lOE := outerr.NewRecordingOutErr()
	exitCode, err := pool.Run(ctx, cCtx, a.cmd, a.lbls, lOE, lr)
	if errors.Is(err, context.Canceled) {
		// Local did not run due to intentional context cancelation.
		return raceResult{t: canceled}
	}
	if exitCode == 0 && err != nil {
		// An unexpected local error occured, report to caller.
		return raceResult{t: canceled, res: command.NewLocalErrorResult(err)}
	}
	// Local ran successfully (even if non-zero exit code). At this point, we use
	// the local result regardless of remote. Hence the below update to the action
	// metadata.
	a.res = command.NewResultFromExitCode(exitCode)
	if a.rec.GetLocalMetadata() == nil {
		a.rec.LocalMetadata = &lpb.LocalMetadata{}
	}
	a.rec.LocalMetadata.ExecutedLocally = true
	a.rec.CopyEventTimesFrom(lr)
	a.rec.LocalMetadata.Result = command.ResultToProto(a.res)
	return raceResult{t: local, res: a.res, oe: lOE}
}

func (a *action) getPreExecOutsInfo() (map[string]filemetadata.Metadata, error) {
	absPath := filepath.Join(a.cmd.ExecRoot, a.cmd.WorkingDir)
	filesInfo := map[string]filemetadata.Metadata{}
	getFileInfo := func(path string, info fs.FileInfo) {
		// fmc is not used here since there is no guarantee that the files on disk for this case would be
		// the same as the files in the cache.
		d, err := digest.NewFromFile(path)
		if err != nil {
			log.Warningf("%v: Failed to get digest for an existing output %v. Output mtime may NOT be preserved for this file: %v",
				a.cmd.Identifiers.ExecutionID, path, err)
			return
		}
		t := info.ModTime()
		relPath, _ := filepath.Rel(absPath, path)
		filesInfo[relPath] = filemetadata.Metadata{Digest: d, MTime: t}
	}

	for _, f := range a.cmd.OutputFiles {
		path := filepath.Join(absPath, f)
		if info, err := os.Stat(path); err == nil {
			getFileInfo(path, info)
		} else if !os.IsNotExist(err) {
			log.Warningf("%v: Failed to stat an existing output. Output mtime may NOT be preserved for this file: %v", a.cmd.Identifiers.ExecutionID, err)
		}
	}
	walkFunc := func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			log.Warningf("%v: Could not walk %v. Output mtime may NOT be preserved for this file: %v", a.cmd.Identifiers.ExecutionID, path, err)
			return nil
		}
		getFileInfo(path, info)
		return nil
	}
	for _, dir := range a.cmd.OutputDirs {
		path := filepath.Join(absPath, dir)
		if err := filepath.Walk(path, walkFunc); err != nil {
			log.Warningf("%v: Could not walk %v. Output mtime may NOT be preserved for this file: %v", a.cmd.Identifiers.ExecutionID, path, err)
		}
	}
	return filesInfo, nil
}

func (a *action) restoreUnchangedOutputMtimes(preExecOutsInfo map[string]filemetadata.Metadata) error {
	outDigests, err := a.execContext.GetOutputFileDigests(false)
	if err != nil {
		return fmt.Errorf("%v: Could not get output digests. Unchanged output mtimes may NOT be preserved: %v", a.cmd.Identifiers.ExecutionID, err)
	}
	for out, d := range outDigests {
		if fInfo, ok := preExecOutsInfo[out]; ok && fInfo.Digest == d {
			if err = os.Chtimes(filepath.Join(a.cmd.ExecRoot, a.cmd.WorkingDir, out), fInfo.MTime, fInfo.MTime); err != nil {
				log.Warningf("%v: Unable to restore mtime of %v. Output mtime may NOT be preserved for this file: %v",
					a.cmd.Identifiers.ExecutionID, out, err)
				return nil
			}
			absPath := filepath.Join(a.cmd.ExecRoot, a.cmd.WorkingDir, out)
			m := a.fmc.Get(absPath)
			m.Digest = fInfo.Digest
			m.MTime = fInfo.MTime
			if err = a.fmc.Update(absPath, m); err != nil {
				log.Warningf("%v: Failed to update filemetadata cache for %v: %v",
					a.cmd.Identifiers.ExecutionID, absPath, err)
			}
		}
	}
	return nil
}

func (a *action) moveOutputsFromTemp(tmpDir string) error {
	dirs := make(map[string]bool)
	srcDir := filepath.Join(tmpDir, a.cmd.WorkingDir)
	destDir := filepath.Join(a.cmd.ExecRoot, a.cmd.WorkingDir)
	for _, f := range a.cmd.OutputFiles {
		src := filepath.Join(srcDir, f)
		if md := a.fmc.Get(src); md.Err != nil {
			log.Errorf("Failed to get file metadata for %v: %v", src, md)
			continue
		}
		dest := filepath.Join(destDir, f)
		if _, ok := dirs[filepath.Dir(dest)]; !ok {
			if err := os.MkdirAll(filepath.Dir(dest), os.ModePerm); err != nil {
				return fmt.Errorf("%v: failed to create output directory %v: %v", a.cmd.Identifiers.ExecutionID, filepath.Dir(dest), err)
			}
			dirs[filepath.Dir(dest)] = true
		}
		if err := os.Rename(src, dest); err != nil {
			return fmt.Errorf("%v: failed to move file %v from %v to %v: %v", a.cmd.Identifiers.ExecutionID, f, srcDir, destDir, err)
		}
	}
	for _, d := range a.cmd.OutputDirs {
		src := filepath.Join(srcDir, d)
		if md := a.fmc.Get(src); !md.IsDirectory {
			continue
		}
		dest := filepath.Join(destDir, d)
		if err := os.RemoveAll(dest); err != nil {
			return fmt.Errorf("%v: failed to remove directory %v: %v", a.cmd.Identifiers.ExecutionID, dest, err)
		}
		if err := os.Rename(src, dest); err != nil {
			return fmt.Errorf("%v: cannot move directory %v from %v to %v: %v", a.cmd.Identifiers.ExecutionID, d, srcDir, destDir, err)
		}
	}
	return nil
}

func (a *action) excludeUnchangedOutputs(outs map[string]*client.TreeOutput, outDir string) map[string]*client.TreeOutput {
	filteredOuts := map[string]*client.TreeOutput{}
	destDir := filepath.Join(outDir, a.cmd.WorkingDir)
	for path, node := range outs {
		dest := filepath.Join(destDir, path)
		// We use os.Stat here instead of fmc.Get since we cannot guarantee that remote-apis-sdks will always update
		// the fmc for new outputs.
		if _, err := os.Stat(dest); err == nil {
			destDigest, err := digest.NewFromFile(dest)
			if err != nil {
				log.Warningf("%v: Failed to get digest for an existing output, downloading new output: %v",
					a.cmd.Identifiers.ExecutionID, err)
				filteredOuts[path] = node
			} else if node.Digest.Hash != destDigest.Hash {
				filteredOuts[path] = node
			}
		} else {
			if !os.IsNotExist(err) {
				log.Warningf("%v: Failed to access existing output, downloading new output: %v",
					a.cmd.Identifiers.ExecutionID, err)
			}
			filteredOuts[path] = node
		}
	}
	return filteredOuts
}

func (a *action) getCachedResult(ctx context.Context) {
	if a.execContext == nil {
		log.Warningf("%v: no rexec.Context", a.cmd.Identifiers.ExecutionID)
		return
	}
	a.execContext.GetCachedResult()
	a.rec.RemoteMetadata = logger.CommandRemoteMetadataToProto(a.execContext.Metadata)
	a.res = a.execContext.Result
	if a.execContext.Result == nil || !a.execContext.Result.IsOk() {
		return
	}
	a.rec.RemoteMetadata.CacheHit = true
	a.rec.RemoteMetadata.Result = command.ResultToProto(a.execContext.Result)
	if !a.cachedResultValid() {
		a.res = command.NewLocalErrorResult(fmt.Errorf("%v failed deps validation", a.cmd.Identifiers.ExecutionID))
		return
	}
	a.rec.LocalMetadata.ValidCacheHit = true
}

func (a *action) cacheLocal() {
	if a.execContext == nil {
		log.Warningf("%v: no rexec.Context", a.cmd.Identifiers.ExecutionID)
		return
	}
	if !a.res.IsOk() || a.lOpt.GetDoNotCache() {
		return
	}
	if err := a.generateDepsFile(); err != nil {
		log.Warningf("%v: Failed to generate deps file: %v", a.cmd.Identifiers.ExecutionID, err)
		return
	}
	// Clear cache entries of output files / output files in output directories in the FMC
	// before updating cached results to RBE.
	// The file metadata cache entries need to be cleared since we did a local run
	// of the action that could potentially have changed the file contents.
	a.clearOutputsCache()
	a.execContext.UpdateCachedResult()
	if a.execContext.Result.Err != nil {
		log.Warningf("%v: Failed updating remote cache: %+v", a.cmd.Identifiers.ExecutionID, a.execContext.Result)
		return
	}
	// Update RemoteMetadata with latest metadata coming from UpdateCachedResult.
	// It will contain event times for updating the cached result, as well as output metrics.
	rm := a.rec.RemoteMetadata
	a.rec.RemoteMetadata = logger.CommandRemoteMetadataToProto(a.execContext.Metadata)
	if rm != nil {
		a.rec.RemoteMetadata.Result = rm.Result
		a.rec.RemoteMetadata.CacheHit = rm.CacheHit
	}
	a.rec.LocalMetadata.UpdatedCache = true
}

func (a *action) populateCommandIO(ctx context.Context, ip *inputprocessor.InputProcessor) error {
	options := &inputprocessor.ProcessInputsOptions{
		ExecutionID:     a.cmd.Identifiers.ExecutionID,
		Cmd:             a.cmd.Args,
		WorkingDir:      a.cmd.WorkingDir,
		ExecRoot:        a.cmd.ExecRoot,
		Inputs:          a.cmd.InputSpec,
		Labels:          a.lbls,
		ToolchainInputs: a.toolchainInputs,
		WindowsCross:    a.windowsCross,
		ExecStrategy:    a.execStrategy,
		CmdEnvironment:  a.cmdEnvironment,
	}
	cmdIO, err := ip.ProcessInputs(ctx, options, a.rec)
	if err != nil {
		err = fmt.Errorf("%v: processing inputs failed: %w. Input options: %+v",
			a.cmd.Identifiers.ExecutionID, err, options)
		a.res = command.NewLocalErrorResult(err)
		return err
	}
	log.V(2).Infof("%v: ProcessInputs returned, inputs: %+v, outputs: %+v", a.cmd.Identifiers.ExecutionID, cmdIO.InputSpec, append(cmdIO.OutputFiles, cmdIO.OutputDirectories...))
	a.cmd.InputSpec = cmdIO.InputSpec
	a.cmd.OutputFiles = pathtranslator.ListRelToWorkingDir(options.ExecRoot, options.WorkingDir, dedup(append(a.cmd.OutputFiles, cmdIO.OutputFiles...)))
	a.cmd.OutputDirs = pathtranslator.ListRelToWorkingDir(options.ExecRoot, options.WorkingDir, dedup(append(a.cmd.OutputDirs, cmdIO.OutputDirectories...)))
	if a.windowsCross {
		a.cmd.WorkingDir = filepath.ToSlash(a.cmd.WorkingDir)
		a.cmd.RemoteWorkingDir = filepath.ToSlash(a.cmd.RemoteWorkingDir)
		for i, p := range a.cmd.OutputFiles {
			a.cmd.OutputFiles[i] = filepath.ToSlash(p)
		}
		for i, p := range a.cmd.OutputDirs {
			a.cmd.OutputDirs[i] = filepath.ToSlash(p)
		}
	}
	if cmdIO.UsedShallowMode {
		// If shallow mode is used, the inferred inputs might not be hermetic, so
		// we store the dependency file for additional deps file validation.
		a.dFile = cmdIO.EmittedDependencyFile
		if a.dFile != "" {
			a.depsFile = a.dFile + ".deps"
		}
	}
	log.V(2).Infof("%v: InputSpec: %+v, outputs: %+v", a.cmd.Identifiers.ExecutionID, a.cmd.InputSpec, append(a.cmd.OutputFiles, a.cmd.OutputDirs...))
	return nil
}

func (a *action) addDepsFileOutput() {
	if a.depsFile != "" {
		a.cmd.OutputFiles = append(a.cmd.OutputFiles, a.depsFile)
	}
}

func (a *action) generateDepsFile() error {
	if a.parser == nil {
		a.createParser()
	}
	if a.dFile != "" {
		// Create the updated deps file.
		if err := a.parser.WriteDepsFile(a.dFile, a.rec); err != nil {
			return err
		}
	}
	return nil
}

func (a *action) clearOutputsCache() {
	if a.fmc == nil {
		return
	}
	files := fileList(filepath.Join(a.cmd.ExecRoot, a.cmd.WorkingDir), a.cmd.OutputFiles, a.cmd.OutputDirs)
	for _, path := range files {
		if log.V(3) {
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				log.Infof("%v: failed to find cache entry for %v while clearing output cache: file does not exist", a.cmd.Identifiers.ExecutionID, path)
			}
		}
		if err := a.fmc.Delete(path); err != nil {
			log.Warningf("%v: failed to delete cache entry of %v while clearing output cache", a.cmd.Identifiers.ExecutionID, path)
		}
	}
}

func (a *action) createExecContext(ctx context.Context, client *rexec.Client) error {
	if a.execContext != nil {
		return nil
	}
	var err error
	if a.execContext, err = client.NewContext(ctx, a.cmd, execOptionsFromProto(a.rOpt), a.oe); err != nil {
		return err
	}
	return nil
}

func (a *action) createParser() {
	if a.parser != nil {
		return
	}
	a.parser = &deps.Parser{ExecRoot: a.cmd.ExecRoot, DigestStore: a.fmc}
}

func (a *action) cachedResultValid() bool {
	log.V(2).Infof("%v: Found cached result", a.cmd.Identifiers.ExecutionID)
	if a.dFile == "" {
		return true
	}
	if a.parser == nil {
		a.createParser()
	}
	ok, err := a.parser.VerifyDepsFile(a.dFile, a.rec)
	if err != nil {
		log.Errorf("%v:  Failed to verify deps file: %v", a.cmd.Identifiers.ExecutionID, err)
		return false
	}
	return ok
}

func (a *action) removeAllOutputs() {
	for _, f := range a.cmd.OutputFiles {
		if f == a.depsFile {
			continue
		}
		path := filepath.Join(a.cmd.ExecRoot, a.cmd.WorkingDir, f)
		if err := os.Remove(path); err != nil {
			log.Warningf("%v: Failed to remove file %v during compare mode: %v", a.cmd.Identifiers.ExecutionID, f, err)
		}
	}
	for _, f := range a.cmd.OutputDirs {
		path := filepath.Join(a.cmd.ExecRoot, a.cmd.WorkingDir, f)
		// Don't recreate the directory if the directory does not exist
		info, err := os.Stat(path)
		if err != nil {
			log.Warningf("%v: Failed to stat directory %v during compare mode: %v", a.cmd.Identifiers.ExecutionID, path, err)
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			log.Warningf("%v: Failed to remove directory %v during compare mode: %v", a.cmd.Identifiers.ExecutionID, path, err)
			continue
		}
		// Recreate the directory in case it is needed for local execution.
		if err := os.MkdirAll(path, info.Mode()); err != nil {
			log.Warningf("%v: Cannot recreate directory %v during compare mode: %v", a.cmd.Identifiers.ExecutionID, f, err)
		}
	}
}

// inOutFiles calculates the list of files contained both in the input and output lists lazily.
// Inputs are expected to be relative to the exec root and Outputs are expected to be relative
// to the working directory. The resulting list of paths are absolute paths.
func (a *action) inOutFiles() []string {
	if a.rawInOutFiles == nil {
		inps := make(map[string]bool, len(a.cmd.InputSpec.Inputs))
		for _, inp := range a.cmd.InputSpec.Inputs {
			inps[inp] = true
		}
		a.rawInOutFiles = []string{}
		for _, f := range pathtranslator.ListRelToExecRoot(a.cmd.ExecRoot, a.cmd.WorkingDir, a.cmd.OutputFiles) {
			if inps[f] {
				a.rawInOutFiles = append(a.rawInOutFiles, filepath.Join(a.cmd.ExecRoot, f))
			}
		}
	}
	return a.rawInOutFiles
}

type restoreInOutFilesFn func()

func (a *action) stashInputOutputFiles() restoreInOutFilesFn {
	restoreFn := StashFiles(a.inOutFiles())
	return func() {
		restoreFn()
		a.clearInputOutputFileCache()
	}

}

func (a *action) clearInputOutputFileCache() {
	if a.fmc == nil {
		return
	}
	for _, path := range a.inOutFiles() {
		if log.V(3) {
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				log.Infof("%v: failed to find file for cache entry of %v while restoring input output files", a.cmd.Identifiers.ExecutionID, path)
			}
		}
		if err := a.fmc.Delete(path); err != nil {
			log.Warningf("%v: failed to delete cache entry of %v while restoring input output files", a.cmd.Identifiers.ExecutionID, path)
		}
	}
}

// Duplicate function is not thread safe.
func (a *action) duplicate(n int) []*action {
	var res []*action
	if a == nil {
		return nil
	}
	for i := 0; i < n; i++ {
		var tcmd command.Command
		if a.cmd != nil {
			tcmd = command.Command(*(a.cmd))
		}

		var tforecast Forecast
		if a.forecast != nil {
			tforecast = Forecast(*(a.forecast))
		}
		trec := logger.NewLogRecord()
		if a.rec != nil {
			trec.LocalMetadata = &lpb.LocalMetadata{
				EventTimes: map[string]*cpb.TimeInterval{
					logger.EventProxyExecution: command.TimeIntervalToProto(&command.TimeInterval{From: time.Now()}),
				},
				Environment: a.rec.LocalMetadata.Environment,
				Labels:      a.rec.LocalMetadata.GetLabels(),
			}
		}
		var trOpt ppb.RemoteExecutionOptions
		if a.rOpt != nil {
			trOpt = ppb.RemoteExecutionOptions(*(a.rOpt))
		}

		var tlOpt ppb.LocalExecutionOptions
		if a.lOpt != nil {
			tlOpt = ppb.LocalExecutionOptions(*(a.lOpt))
		}

		newAction := &action{}
		*newAction = *a
		newAction.cmd = &tcmd
		newAction.forecast = &tforecast
		newAction.rec = trec
		newAction.rOpt = &trOpt
		newAction.lOpt = &tlOpt
		newAction.oe = outerr.NewRecordingOutErr()
		res = append(res, newAction)
	}
	return res
}