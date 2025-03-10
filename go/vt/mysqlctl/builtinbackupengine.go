/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mysqlctl

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/pflag"
	"golang.org/x/sync/semaphore"

	"vitess.io/vitess/go/ioutil"
	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/vt/concurrency"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/logutil"
	stats "vitess.io/vitess/go/vt/mysqlctl/backupstats"
	"vitess.io/vitess/go/vt/mysqlctl/backupstorage"
	"vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/servenv"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/topoproto"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vttablet/tmclient"
)

const (
	builtinBackupEngineName = "builtin"
	autoIncrementalFromPos  = "auto"
	dataDictionaryFile      = "mysql.ibd"
)

var (
	// BuiltinBackupMysqldTimeout is how long ExecuteBackup should wait for response from mysqld.Shutdown.
	// It can later be extended for other calls to mysqld during backup functions.
	// Exported for testing.
	BuiltinBackupMysqldTimeout = 10 * time.Minute

	builtinBackupProgress = 5 * time.Second

	// Controls the size of the IO buffer used when reading files during backups.
	builtinBackupFileReadBufferSize uint

	// Controls the size of the IO buffer used when writing files during restores.
	builtinBackupFileWriteBufferSize uint = 2 * 1024 * 1024 /* 2 MiB */

	// Controls the size of the IO buffer used when writing to backupstorage
	// engines during backups.  The backupstorage may be a physical file,
	// network, or something else.
	builtinBackupStorageWriteBufferSize = 2 * 1024 * 1024 /* 2 MiB */
)

// BuiltinBackupEngine encapsulates the logic of the builtin engine
// it implements the BackupEngine interface and contains all the logic
// required to implement a backup/restore by copying files from and to
// the correct location / storage bucket
type BuiltinBackupEngine struct {
}

// builtinBackupManifest represents the backup. It lists all the files, the
// Position that the backup was taken at, the compression engine used, etc.
type builtinBackupManifest struct {
	// BackupManifest is an anonymous embedding of the base manifest struct.
	BackupManifest

	// CompressionEngine stores which compression engine was originally provided
	// to compress the files. Please note that if user has provided externalCompressorCmd
	// then it will contain value 'external'. This field is used during restore routine to
	// get a hint about what kind of compression was used.
	CompressionEngine string `json:",omitempty"`

	// FileEntries contains all the files in the backup
	FileEntries []FileEntry

	// SkipCompress is true if the backup files were NOT run through gzip.
	// The field is expressed as a negative because it will come through as
	// false for backups that were created before the field existed, and those
	// backups all had compression enabled.
	SkipCompress bool

	// When CompressionEngine is "external", ExternalDecompressor may be
	// consulted for the external decompressor command.
	//
	// When taking a backup with --compression-engine=external,
	// ExternalDecompressor will be set to the value of
	// --manifest-external-decompressor, if set, or else left as an empty
	// string.
	//
	// When restoring from a backup with CompressionEngine "external",
	// --external-decompressor will be consulted first and, if that is not set,
	// ExternalDecompressor will be used. If neither are set, the restore will
	// abort.
	ExternalDecompressor string
}

// FileEntry is one file to backup
type FileEntry struct {
	// Base is one of:
	// - backupInnodbDataHomeDir for files that go into Mycnf.InnodbDataHomeDir
	// - backupInnodbLogGroupHomeDir for files that go into Mycnf.InnodbLogGroupHomeDir
	// - binLogDir for files that go in the binlog dir (base path of Mycnf.BinLogPath)
	// - backupData for files that go into Mycnf.DataDir
	Base string

	// Name is the file name, relative to Base
	Name string

	// Hash is the hash of the final data (transformed and
	// compressed if specified) stored in the BackupStorage.
	Hash string

	// ParentPath is an optional prefix to the Base path. If empty, it is ignored. Useful
	// for writing files in a temporary directory
	ParentPath string
}

func init() {
	for _, cmd := range []string{"vtbackup", "vtcombo", "vttablet", "vttestserver", "vtctld", "vtctldclient"} {
		servenv.OnParseFor(cmd, registerBuiltinBackupEngineFlags)
	}
}

func registerBuiltinBackupEngineFlags(fs *pflag.FlagSet) {
	fs.DurationVar(&BuiltinBackupMysqldTimeout, "builtinbackup_mysqld_timeout", BuiltinBackupMysqldTimeout, "how long to wait for mysqld to shutdown at the start of the backup.")
	fs.DurationVar(&builtinBackupProgress, "builtinbackup_progress", builtinBackupProgress, "how often to send progress updates when backing up large files.")
	fs.UintVar(&builtinBackupFileReadBufferSize, "builtinbackup-file-read-buffer-size", builtinBackupFileReadBufferSize, "read files using an IO buffer of this many bytes. Golang defaults are used when set to 0.")
	fs.UintVar(&builtinBackupFileWriteBufferSize, "builtinbackup-file-write-buffer-size", builtinBackupFileWriteBufferSize, "write files using an IO buffer of this many bytes. Golang defaults are used when set to 0.")
}

// isIncrementalBackup is a convenience function to check whether the params indicate an incremental backup request
func isIncrementalBackup(params BackupParams) bool {
	return params.IncrementalFromPos != ""
}

// fullPath returns the full path of the entry, based on its type
func (fe *FileEntry) fullPath(cnf *Mycnf) (string, error) {
	// find the root to use
	var root string
	switch fe.Base {
	case backupInnodbDataHomeDir:
		root = cnf.InnodbDataHomeDir
	case backupInnodbLogGroupHomeDir:
		root = cnf.InnodbLogGroupHomeDir
	case backupData:
		root = cnf.DataDir
	case backupBinlogDir:
		root = filepath.Dir(cnf.BinLogPath)
	default:
		return "", vterrors.Errorf(vtrpc.Code_UNKNOWN, "unknown base: %v", fe.Base)
	}

	return path.Join(fe.ParentPath, root, fe.Name), nil
}

// open attempts t oopen the file
func (fe *FileEntry) open(cnf *Mycnf, readOnly bool) (*os.File, error) {
	name, err := fe.fullPath(cnf)
	if err != nil {
		return nil, vterrors.Wrapf(err, "cannot evaluate full name for %v", fe.Name)
	}
	var fd *os.File
	if readOnly {
		if fd, err = os.Open(name); err != nil {
			return nil, vterrors.Wrapf(err, "cannot open source file %v", name)
		}
	} else {
		dir := path.Dir(name)
		if err := os.MkdirAll(dir, os.ModePerm); err != nil {
			return nil, vterrors.Wrapf(err, "cannot create destination directory %v", dir)
		}
		if fd, err = os.Create(name); err != nil {
			return nil, vterrors.Wrapf(err, "cannot create destination file %v", name)
		}
	}
	return fd, nil
}

// ExecuteBackup runs a backup based on given params. This could be a full or incremental backup.
// The function returns a boolean that indicates if the backup is usable, and an overall error.
func (be *BuiltinBackupEngine) ExecuteBackup(ctx context.Context, params BackupParams, bh backupstorage.BackupHandle) (bool, error) {
	params.Logger.Infof("Executing Backup at %v for keyspace/shard %v/%v on tablet %v, concurrency: %v, compress: %v, incrementalFromPos: %v",
		params.BackupTime, params.Keyspace, params.Shard, params.TabletAlias, params.Concurrency, backupStorageCompress, params.IncrementalFromPos)

	if isIncrementalBackup(params) {
		return be.executeIncrementalBackup(ctx, params, bh)
	}
	return be.executeFullBackup(ctx, params, bh)
}

// executeIncrementalBackup runs an incremental backup, based on given 'incremental_from_pos', which can be:
// - A valid position
// - "auto", indicating the incremental backup should begin with last successful backup end position.
func (be *BuiltinBackupEngine) executeIncrementalBackup(ctx context.Context, params BackupParams, bh backupstorage.BackupHandle) (bool, error) {
	// Collect MySQL status:
	// UUID
	serverUUID, err := params.Mysqld.GetServerUUID(ctx)
	if err != nil {
		return false, vterrors.Wrap(err, "can't get server uuid")
	}
	// @@gtid_purged
	getPurgedGTIDSet := func() (mysql.Mysql56GTIDSet, error) {
		gtidPurged, err := params.Mysqld.GetGTIDPurged(ctx)
		if err != nil {
			return nil, vterrors.Wrap(err, "can't get @@gtid_purged")
		}
		purgedGTIDSet, ok := gtidPurged.GTIDSet.(mysql.Mysql56GTIDSet)
		if !ok {
			return nil, vterrors.Errorf(vtrpc.Code_FAILED_PRECONDITION, "cannot get MySQL GTID purged value: %v", gtidPurged)
		}
		return purgedGTIDSet, nil
	}
	purgedGTIDSet, err := getPurgedGTIDSet()
	if err != nil {
		return false, err
	}

	if params.IncrementalFromPos == autoIncrementalFromPos {
		params.Logger.Infof("auto evaluating incremental_from_pos")
		bs, err := backupstorage.GetBackupStorage()
		if err != nil {
			return false, err
		}
		defer bs.Close()

		// Backups are stored in a directory structure that starts with
		// <keyspace>/<shard>
		backupDir := GetBackupDir(params.Keyspace, params.Shard)
		bhs, err := bs.ListBackups(ctx, backupDir)
		if err != nil {
			return false, vterrors.Wrap(err, "ListBackups failed")
		}
		_, manifest, err := FindLatestSuccessfulBackup(ctx, params.Logger, bhs)
		if err != nil {
			return false, vterrors.Wrap(err, "FindLatestSuccessfulBackup failed")
		}
		params.IncrementalFromPos = mysql.EncodePosition(manifest.Position)
		params.Logger.Infof("auto evaluated incremental_from_pos: %s", params.IncrementalFromPos)
	}

	// params.IncrementalFromPos is a string. We want to turn that into a MySQL GTID
	getIncrementalFromPosGTIDSet := func() (mysql.Mysql56GTIDSet, error) {
		pos, err := mysql.DecodePosition(params.IncrementalFromPos)
		if err != nil {
			return nil, vterrors.Wrapf(err, "cannot decode position in incremental backup: %v", params.IncrementalFromPos)
		}
		if !pos.MatchesFlavor(mysql.Mysql56FlavorID) {
			return nil, vterrors.Errorf(vtrpc.Code_FAILED_PRECONDITION, "incremental backup only supports MySQL GTID positions. Got: %v", params.IncrementalFromPos)
		}
		ifPosGTIDSet, ok := pos.GTIDSet.(mysql.Mysql56GTIDSet)
		if !ok {
			return nil, vterrors.Errorf(vtrpc.Code_FAILED_PRECONDITION, "cannot get MySQL GTID value: %v", pos)
		}
		return ifPosGTIDSet, nil
	}
	backupFromGTIDSet, err := getIncrementalFromPosGTIDSet()
	if err != nil {
		return false, err
	}
	// OK, we now have the formal MySQL GTID from which we want to take the incremental backip.

	// binlogs may not contain information about purged GTIDs. e.g. some binlog.000003 may have
	// previous GTIDs like 00021324-1111-1111-1111-111111111111:30-60, ie 1-29 range is missing. This can happen
	// when a server is restored from backup and set with gtid_purged != "".
	// This is fine!
	// Shortly we will compare a binlog's "Previous GTIDs" with the backup's position. For the purpose of comparison, we
	// ignore the purged GTIDs:

	if err := params.Mysqld.FlushBinaryLogs(ctx); err != nil {
		return false, vterrors.Wrapf(err, "cannot flush binary logs in incremental backup")
	}
	binaryLogs, err := params.Mysqld.GetBinaryLogs(ctx)
	if err != nil {
		return false, vterrors.Wrapf(err, "cannot get binary logs in incremental backup")
	}
	previousGTIDs := map[string]string{}
	getBinlogPreviousGTIDs := func(ctx context.Context, binlog string) (gtids string, err error) {
		gtids, ok := previousGTIDs[binlog]
		if ok {
			// Found a cached entry! No need to query again
			return gtids, nil
		}
		gtids, err = params.Mysqld.GetPreviousGTIDs(ctx, binlog)
		if err != nil {
			return gtids, err
		}
		previousGTIDs[binlog] = gtids
		return gtids, nil
	}
	binaryLogsToBackup, incrementalBackupFromGTID, incrementalBackupToGTID, err := ChooseBinlogsForIncrementalBackup(ctx, backupFromGTIDSet, purgedGTIDSet, binaryLogs, getBinlogPreviousGTIDs)
	if err != nil {
		return false, vterrors.Wrapf(err, "cannot get binary logs to backup in incremental backup")
	}
	incrementalBackupFromPosition, err := mysql.ParsePosition(mysql.Mysql56FlavorID, incrementalBackupFromGTID)
	if err != nil {
		return false, vterrors.Wrapf(err, "cannot parse position %v", incrementalBackupFromGTID)
	}
	incrementalBackupToPosition, err := mysql.ParsePosition(mysql.Mysql56FlavorID, incrementalBackupToGTID)
	if err != nil {
		return false, vterrors.Wrapf(err, "cannot parse position %v", incrementalBackupToGTID)
	}
	// It's worthwhile we explain the difference between params.IncrementalFromPos and incrementalBackupFromPosition.
	// params.IncrementalFromPos is supplied by the user. They want an incremental backup that covers that position.
	// However, we implement incremental backups by copying complete binlog files. That position could potentially
	// be somewhere in the middle of some binlog. So we look at the earliest binlog file that covers the user's position.
	// The backup we take either starts exactly at the user's position or at some prior position, depending where in the
	// binlog file the user's requested position is found.
	// incrementalBackupFromGTID is the "previous GTIDs" of the first binlog file we back up.
	// It is a fact that incrementalBackupFromGTID is earlier or equal to params.IncrementalFromPos.
	// In the backup manifest file, we document incrementalBackupFromGTID, not the user's requested position.
	if err := be.backupFiles(ctx, params, bh, incrementalBackupToPosition, mysql.Position{}, incrementalBackupFromPosition, binaryLogsToBackup, serverUUID); err != nil {
		return false, err
	}
	return true, nil
}

// executeFullBackup returns a boolean that indicates if the backup is usable,
// and an overall error.
func (be *BuiltinBackupEngine) executeFullBackup(ctx context.Context, params BackupParams, bh backupstorage.BackupHandle) (bool, error) {

	if params.IncrementalFromPos != "" {
		return be.executeIncrementalBackup(ctx, params, bh)
	}

	// Save initial state so we can restore.
	replicaStartRequired := false
	sourceIsPrimary := false
	superReadOnly := true //nolint
	readOnly := true      //nolint
	var replicationPosition mysql.Position
	semiSyncSource, semiSyncReplica := params.Mysqld.SemiSyncEnabled()

	// See if we need to restart replication after backup.
	params.Logger.Infof("getting current replication status")
	replicaStatus, err := params.Mysqld.ReplicationStatus()
	switch err {
	case nil:
		replicaStartRequired = replicaStatus.Healthy() && !DisableActiveReparents
	case mysql.ErrNotReplica:
		// keep going if we're the primary, might be a degenerate case
		sourceIsPrimary = true
	default:
		return false, vterrors.Wrap(err, "can't get replica status")
	}

	// get the read-only flag
	readOnly, err = params.Mysqld.IsReadOnly()
	if err != nil {
		return false, vterrors.Wrap(err, "failed to get read_only status")
	}
	superReadOnly, err = params.Mysqld.IsSuperReadOnly()
	if err != nil {
		return false, vterrors.Wrap(err, "can't get super_read_only status")
	}
	log.Infof("Flag values during full backup, read_only: %v, super_read_only:%t", readOnly, superReadOnly)

	// get the replication position
	if sourceIsPrimary {
		// No need to set read_only because super_read_only will implicitly set read_only to true as well.
		if !superReadOnly {
			params.Logger.Infof("Enabling super_read_only on primary prior to backup")
			if _, err = params.Mysqld.SetSuperReadOnly(true); err != nil {
				return false, vterrors.Wrap(err, "failed to enable super_read_only")
			}
			defer func() {
				// Resetting super_read_only back to its original value
				params.Logger.Infof("resetting mysqld super_read_only to %v", superReadOnly)
				if _, err := params.Mysqld.SetSuperReadOnly(false); err != nil {
					log.Error("Failed to set super_read_only back to its original value")
				}
			}()

		}
		replicationPosition, err = params.Mysqld.PrimaryPosition()
		if err != nil {
			return false, vterrors.Wrap(err, "can't get position on primary")
		}
	} else {
		// This is a replica
		if err := params.Mysqld.StopReplication(params.HookExtraEnv); err != nil {
			return false, vterrors.Wrapf(err, "can't stop replica")
		}
		replicaStatus, err := params.Mysqld.ReplicationStatus()
		if err != nil {
			return false, vterrors.Wrap(err, "can't get replica status")
		}
		replicationPosition = replicaStatus.Position
	}
	params.Logger.Infof("using replication position: %v", replicationPosition)

	gtidPurgedPosition, err := params.Mysqld.GetGTIDPurged(ctx)
	if err != nil {
		return false, vterrors.Wrap(err, "can't get gtid_purged")
	}

	if err != nil {
		return false, vterrors.Wrap(err, "can't get purged position")
	}

	serverUUID, err := params.Mysqld.GetServerUUID(ctx)
	if err != nil {
		return false, vterrors.Wrap(err, "can't get server uuid")
	}

	// shutdown mysqld
	shutdownCtx, cancel := context.WithTimeout(ctx, BuiltinBackupMysqldTimeout)
	err = params.Mysqld.Shutdown(shutdownCtx, params.Cnf, true)
	defer cancel()
	if err != nil {
		return false, vterrors.Wrap(err, "can't shutdown mysqld")
	}

	// Backup everything, capture the error.
	backupErr := be.backupFiles(ctx, params, bh, replicationPosition, gtidPurgedPosition, mysql.Position{}, nil, serverUUID)
	usable := backupErr == nil

	// Try to restart mysqld, use background context in case we timed out the original context
	err = params.Mysqld.Start(context.Background(), params.Cnf)
	if err != nil {
		return usable, vterrors.Wrap(err, "can't restart mysqld")
	}

	// Resetting super_read_only back to its original value
	params.Logger.Infof("resetting mysqld super_read_only to %v", superReadOnly)
	if _, err := params.Mysqld.SetSuperReadOnly(superReadOnly); err != nil {
		return usable, err
	}

	// Restore original mysqld state that we saved above.
	if semiSyncSource || semiSyncReplica {
		// Only do this if one of them was on, since both being off could mean
		// the plugin isn't even loaded, and the server variables don't exist.
		params.Logger.Infof("restoring semi-sync settings from before backup: primary=%v, replica=%v",
			semiSyncSource, semiSyncReplica)
		err := params.Mysqld.SetSemiSyncEnabled(semiSyncSource, semiSyncReplica)
		if err != nil {
			return usable, err
		}
	}
	if replicaStartRequired {
		params.Logger.Infof("restarting mysql replication")
		if err := params.Mysqld.StartReplication(params.HookExtraEnv); err != nil {
			return usable, vterrors.Wrap(err, "cannot restart replica")
		}

		// this should be quick, but we might as well just wait
		if err := WaitForReplicationStart(params.Mysqld, replicationStartDeadline); err != nil {
			return usable, vterrors.Wrap(err, "replica is not restarting")
		}

		// Wait for a reliable value for ReplicationLagSeconds from ReplicationStatus()

		// We know that we stopped at replicationPosition.
		// If PrimaryPosition is the same, that means no writes
		// have happened to primary, so we are up-to-date.
		// Otherwise, we wait for replica's Position to change from
		// the saved replicationPosition before proceeding
		tmc := tmclient.NewTabletManagerClient()
		defer tmc.Close()
		remoteCtx, remoteCancel := context.WithTimeout(ctx, topo.RemoteOperationTimeout)
		defer remoteCancel()

		pos, err := getPrimaryPosition(remoteCtx, tmc, params.TopoServer, params.Keyspace, params.Shard)
		// If we are unable to get the primary's position, return error.
		if err != nil {
			return usable, err
		}
		if !replicationPosition.Equal(pos) {
			for {
				if err := ctx.Err(); err != nil {
					return usable, err
				}
				status, err := params.Mysqld.ReplicationStatus()
				if err != nil {
					return usable, err
				}
				newPos := status.Position
				if !newPos.Equal(replicationPosition) {
					break
				}
				time.Sleep(1 * time.Second)
			}
		}
	}

	return usable, backupErr
}

// backupFiles finds the list of files to backup, and creates the backup.
func (be *BuiltinBackupEngine) backupFiles(
	ctx context.Context,
	params BackupParams,
	bh backupstorage.BackupHandle,
	replicationPosition mysql.Position,
	purgedPosition mysql.Position,
	fromPosition mysql.Position,
	binlogFiles []string,
	serverUUID string,
) (finalErr error) {

	// Get the files to backup.
	// We don't care about totalSize because we add each file separately.
	var fes []FileEntry
	var err error
	if isIncrementalBackup(params) {
		fes, _, err = binlogFilesToBackup(params.Cnf, binlogFiles)
	} else {
		fes, _, err = findFilesToBackup(params.Cnf)
	}
	if err != nil {
		return vterrors.Wrap(err, "can't find files to backup")
	}
	params.Logger.Infof("found %v files to backup", len(fes))

	// Backup with the provided concurrency.
	sema := semaphore.NewWeighted(int64(params.Concurrency))
	wg := sync.WaitGroup{}
	for i := range fes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fe := &fes[i]
			// Wait until we are ready to go, return if we encounter an error
			acqErr := sema.Acquire(ctx, 1)
			if acqErr != nil {
				log.Errorf("Unable to acquire semaphore needed to backup file: %s, err: %s", fe.Name, acqErr.Error())
				bh.RecordError(acqErr)
				return
			}
			defer sema.Release(1)
			// Check for context cancellation explicitly because, the way semaphore code is written, theoretically we might
			// end up not throwing an error even after cancellation. Please see https://cs.opensource.google/go/x/sync/+/refs/tags/v0.1.0:semaphore/semaphore.go;l=66,
			// which suggests that if the context is already done, `Acquire()` may still succeed without blocking. This introduces
			// unpredictability in my test cases, so in order to avoid that, I am adding this cancellation check.
			select {
			case <-ctx.Done():
				log.Errorf("Context canceled or timed out during %q backup", fe.Name)
				bh.RecordError(vterrors.Errorf(vtrpc.Code_CANCELED, "context canceled"))
				return
			default:
			}

			if bh.HasErrors() {
				params.Logger.Infof("failed to backup files due to error.")
				return
			}

			// Backup the individual file.
			name := fmt.Sprintf("%v", i)
			bh.RecordError(be.backupFile(ctx, params, bh, fe, name))
		}(i)
	}

	wg.Wait()

	// BackupHandle supports the ErrorRecorder interface for tracking errors
	// across any goroutines that fan out to take the backup. This means that we
	// don't need a local error recorder and can put everything through the bh.
	//
	// This handles the scenario where bh.AddFile() encounters an error asynchronously,
	// which ordinarily would be lost in the context of `be.backupFile`, i.e. if an
	// error were encountered
	// [here](https://github.com/vitessio/vitess/blob/d26b6c7975b12a87364e471e2e2dfa4e253c2a5b/go/vt/mysqlctl/s3backupstorage/s3.go#L139-L142).
	if bh.HasErrors() {
		return bh.Error()
	}

	// open the MANIFEST
	wc, err := bh.AddFile(ctx, backupManifestFileName, backupstorage.FileSizeUnknown)
	if err != nil {
		return vterrors.Wrapf(err, "cannot add %v to backup", backupManifestFileName)
	}
	defer func() {
		if closeErr := wc.Close(); finalErr == nil {
			finalErr = closeErr
		}
	}()

	// JSON-encode and write the MANIFEST
	bm := &builtinBackupManifest{
		// Common base fields
		BackupManifest: BackupManifest{
			BackupMethod:   builtinBackupEngineName,
			Position:       replicationPosition,
			PurgedPosition: purgedPosition,
			FromPosition:   fromPosition,
			Incremental:    !fromPosition.IsZero(),
			ServerUUID:     serverUUID,
			TabletAlias:    params.TabletAlias,
			Keyspace:       params.Keyspace,
			Shard:          params.Shard,
			BackupTime:     params.BackupTime.UTC().Format(time.RFC3339),
			FinishedTime:   time.Now().UTC().Format(time.RFC3339),
		},

		// Builtin-specific fields
		FileEntries:          fes,
		SkipCompress:         !backupStorageCompress,
		CompressionEngine:    CompressionEngineName,
		ExternalDecompressor: ManifestExternalDecompressorCmd,
	}
	data, err := json.MarshalIndent(bm, "", "  ")
	if err != nil {
		return vterrors.Wrapf(err, "cannot JSON encode %v", backupManifestFileName)
	}
	if _, err := wc.Write([]byte(data)); err != nil {
		return vterrors.Wrapf(err, "cannot write %v", backupManifestFileName)
	}

	return nil
}

type backupPipe struct {
	filename string
	maxSize  int64

	r io.Reader
	w *bufio.Writer

	crc32  hash.Hash32
	nn     int64
	done   chan struct{}
	closed int32
}

func newBackupWriter(filename string, writerBufferSize int, maxSize int64, w io.Writer) *backupPipe {
	return &backupPipe{
		crc32:    crc32.NewIEEE(),
		w:        bufio.NewWriterSize(w, writerBufferSize),
		filename: filename,
		maxSize:  maxSize,
		done:     make(chan struct{}),
	}
}

func newBackupReader(filename string, maxSize int64, r io.Reader) *backupPipe {
	return &backupPipe{
		crc32:    crc32.NewIEEE(),
		r:        r,
		filename: filename,
		done:     make(chan struct{}),
		maxSize:  maxSize,
	}
}

func (bp *backupPipe) Read(p []byte) (int, error) {
	nn, err := bp.r.Read(p)
	_, _ = bp.crc32.Write(p[:nn])
	atomic.AddInt64(&bp.nn, int64(nn))
	return nn, err
}

func (bp *backupPipe) Write(p []byte) (int, error) {
	nn, err := bp.w.Write(p)
	_, _ = bp.crc32.Write(p[:nn])
	atomic.AddInt64(&bp.nn, int64(nn))
	return nn, err
}

func (bp *backupPipe) Close() error {
	if atomic.CompareAndSwapInt32(&bp.closed, 0, 1) {
		close(bp.done)
		if bp.w != nil {
			if err := bp.w.Flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (bp *backupPipe) HashString() string {
	return hex.EncodeToString(bp.crc32.Sum(nil))
}

func (bp *backupPipe) ReportProgress(period time.Duration, logger logutil.Logger) {
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-bp.done:
			logger.Infof("Done taking Backup %q", bp.filename)
			return
		case <-tick.C:
			written := float64(atomic.LoadInt64(&bp.nn))
			if bp.maxSize == 0 {
				logger.Infof("Backup %q: %.02fkb", bp.filename, written/1024.0)
			} else {
				maxSize := float64(bp.maxSize)
				logger.Infof("Backup %q: %.02f%% (%.02f/%.02fkb)", bp.filename, 100.0*written/maxSize, written/1024.0, maxSize/1024.0)
			}
		}
	}
}

// backupFile backs up an individual file.
func (be *BuiltinBackupEngine) backupFile(ctx context.Context, params BackupParams, bh backupstorage.BackupHandle, fe *FileEntry, name string) (finalErr error) {
	// Open the source file for reading.
	openSourceAt := time.Now()
	source, err := fe.open(params.Cnf, true)
	if err != nil {
		return err
	}
	params.Stats.Scope(stats.Operation("Source:Open")).TimedIncrement(time.Since(openSourceAt))

	defer func() {
		closeSourceAt := time.Now()
		source.Close()
		params.Stats.Scope(stats.Operation("Source:Close")).TimedIncrement(time.Since(closeSourceAt))
	}()

	readStats := params.Stats.Scope(stats.Operation("Source:Read"))
	timedSource := ioutil.NewMeteredReadCloser(source, readStats.TimedIncrementBytes)

	fi, err := source.Stat()
	if err != nil {
		return err
	}

	br := newBackupReader(fe.Name, fi.Size(), timedSource)
	go br.ReportProgress(builtinBackupProgress, params.Logger)

	// Open the destination file for writing, and a buffer.
	params.Logger.Infof("Backing up file: %v", fe.Name)
	openDestAt := time.Now()
	dest, err := bh.AddFile(ctx, name, fi.Size())
	if err != nil {
		return vterrors.Wrapf(err, "cannot add file: %v,%v", name, fe.Name)
	}
	params.Stats.Scope(stats.Operation("Destination:Open")).TimedIncrement(time.Since(openDestAt))

	defer func(name, fileName string) {
		closeDestAt := time.Now()
		if rerr := dest.Close(); rerr != nil {
			if finalErr != nil {
				// We already have an error, just log this one.
				params.Logger.Errorf2(rerr, "failed to close file %v,%v", name, fe.Name)
			} else {
				finalErr = rerr
			}
		}
		params.Stats.Scope(stats.Operation("Destination:Close")).TimedIncrement(time.Since(closeDestAt))
	}(name, fe.Name)

	destStats := params.Stats.Scope(stats.Operation("Destination:Write"))
	timedDest := ioutil.NewMeteredWriteCloser(dest, destStats.TimedIncrementBytes)

	bw := newBackupWriter(fe.Name, builtinBackupStorageWriteBufferSize, fi.Size(), timedDest)

	var reader io.Reader = br
	var writer io.Writer = bw

	// Create the gzip compression pipe, if necessary.
	var compressor io.WriteCloser
	if backupStorageCompress {
		if ExternalCompressorCmd != "" {
			compressor, err = newExternalCompressor(ctx, ExternalCompressorCmd, writer, params.Logger)
		} else {
			compressor, err = newBuiltinCompressor(CompressionEngineName, writer, params.Logger)
		}
		if err != nil {
			return vterrors.Wrap(err, "can't create compressor")
		}

		compressStats := params.Stats.Scope(stats.Operation("Compressor:Write"))
		writer = ioutil.NewMeteredWriter(compressor, compressStats.TimedIncrementBytes)
	}

	if builtinBackupFileReadBufferSize > 0 {
		reader = bufio.NewReaderSize(br, int(builtinBackupFileReadBufferSize))
	}

	// Copy from the source file to writer (optional gzip,
	// optional pipe, tee, output file and hasher).
	_, err = io.Copy(writer, reader)
	if err != nil {
		return vterrors.Wrap(err, "cannot copy data")
	}

	// Close gzip to flush it, after that all data is sent to writer.
	if compressor != nil {
		closeCompressorAt := time.Now()
		if err = compressor.Close(); err != nil {
			return vterrors.Wrap(err, "cannot close compressor")
		}
		params.Stats.Scope(stats.Operation("Compressor:Close")).TimedIncrement(time.Since(closeCompressorAt))
	}

	// Close the backupPipe to finish writing on destination.
	closeWriterAt := time.Now()
	if err = bw.Close(); err != nil {
		return vterrors.Wrapf(err, "cannot flush destination: %v", name)
	}
	params.Stats.Scope(stats.Operation("Destination:Close")).TimedIncrement(time.Since(closeWriterAt))

	closeReaderAt := time.Now()
	if err := br.Close(); err != nil {
		return vterrors.Wrap(err, "failed to close the source reader")
	}
	params.Stats.Scope(stats.Operation("Source:Close")).TimedIncrement(time.Since(closeReaderAt))

	// Save the hash.
	fe.Hash = bw.HashString()
	return nil
}

// executeRestoreFullBackup restores the files from a full backup. The underlying mysql database service is expected to be stopped.
func (be *BuiltinBackupEngine) executeRestoreFullBackup(ctx context.Context, params RestoreParams, bh backupstorage.BackupHandle, bm builtinBackupManifest) error {
	if err := prepareToRestore(ctx, params.Cnf, params.Mysqld, params.Logger); err != nil {
		return err
	}

	params.Logger.Infof("Restore: copying %v files", len(bm.FileEntries))

	if _, err := be.restoreFiles(ctx, params, bh, bm); err != nil {
		// don't delete the file here because that is how we detect an interrupted restore
		return vterrors.Wrap(err, "failed to restore files")
	}
	return nil
}

// executeRestoreIncrementalBackup executes a restore of an incremental backup, and expect to run on top of a full backup's restore.
// It restores any (zero or more) binary log files and applies them onto the underlying database one at a time, but only applies those transactions
// that fall within params.RestoreToPos.GTIDSet. The rest (typically a suffix of the last binary log) are discarded.
// The underlying mysql database is expected to be up and running.
func (be *BuiltinBackupEngine) executeRestoreIncrementalBackup(ctx context.Context, params RestoreParams, bh backupstorage.BackupHandle, bm builtinBackupManifest) error {
	params.Logger.Infof("Restoring incremental backup to position: %v", bm.Position)
	createdDir, err := be.restoreFiles(ctx, params, bh, bm)
	defer os.RemoveAll(createdDir)
	mysqld, ok := params.Mysqld.(*Mysqld)
	if !ok {
		return vterrors.Errorf(vtrpc.Code_UNIMPLEMENTED, "expected: Mysqld")
	}
	for _, fe := range bm.FileEntries {
		fe.ParentPath = createdDir
		binlogFile, err := fe.fullPath(params.Cnf)
		if err != nil {
			return vterrors.Wrap(err, "failed to restore file")
		}
		if err := mysqld.ApplyBinlogFile(ctx, binlogFile, params.RestoreToPos); err != nil {
			return vterrors.Wrap(err, "failed to extract binlog file")
		}
		defer os.Remove(binlogFile)
		params.Logger.Infof("Applied binlog file: %v", binlogFile)
	}
	if err != nil {
		// don't delete the file here because that is how we detect an interrupted restore
		return vterrors.Wrap(err, "failed to restore files")
	}
	params.Logger.Infof("Restored incremental backup files to: %v", createdDir)

	return nil
}

// ExecuteRestore restores from a backup. If the restore is successful
// we return the position from which replication should start
// otherwise an error is returned
func (be *BuiltinBackupEngine) ExecuteRestore(ctx context.Context, params RestoreParams, bh backupstorage.BackupHandle) (*BackupManifest, error) {

	var bm builtinBackupManifest
	if err := getBackupManifestInto(ctx, bh, &bm); err != nil {
		return nil, err
	}

	// mark restore as in progress
	if err := createStateFile(params.Cnf); err != nil {
		return nil, err
	}

	var err error
	if bm.Incremental {
		err = be.executeRestoreIncrementalBackup(ctx, params, bh, bm)
	} else {
		err = be.executeRestoreFullBackup(ctx, params, bh, bm)
	}
	if err != nil {
		return nil, err
	}
	params.Logger.Infof("Restore: returning replication position %v", bm.Position)
	return &bm.BackupManifest, nil
}

// restoreFiles will copy all the files from the BackupStorage to the
// right place.
func (be *BuiltinBackupEngine) restoreFiles(ctx context.Context, params RestoreParams, bh backupstorage.BackupHandle, bm builtinBackupManifest) (createdDir string, err error) {
	// For optimization, we are replacing pargzip with pgzip, so newBuiltinDecompressor doesn't have to compare and print warning for every file
	// since newBuiltinDecompressor is helper method and does not hold any state, it was hard to do it in that method itself.
	if bm.CompressionEngine == PargzipCompressor {
		params.Logger.Warningf(`engine "pargzip" doesn't support decompression, using "pgzip" instead`)
		bm.CompressionEngine = PgzipCompressor
		defer func() {
			bm.CompressionEngine = PargzipCompressor
		}()
	}

	if bm.Incremental {
		createdDir, err = os.MkdirTemp("", "restore-incremental-*")
		if err != nil {
			return "", err
		}
	}
	fes := bm.FileEntries
	sema := semaphore.NewWeighted(int64(params.Concurrency))
	rec := concurrency.AllErrorRecorder{}
	wg := sync.WaitGroup{}
	for i := range fes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fe := &fes[i]
			// Wait until we are ready to go, return if we encounter an error
			acqErr := sema.Acquire(ctx, 1)
			if acqErr != nil {
				log.Errorf("Unable to acquire semaphore needed to restore file: %s, err: %s", fe.Name, acqErr.Error())
				rec.RecordError(acqErr)
				return
			}
			defer sema.Release(1)
			// Check for context cancellation explicitly because, the way semaphore code is written, theoretically we might
			// end up not throwing an error even after cancellation. Please see https://cs.opensource.google/go/x/sync/+/refs/tags/v0.1.0:semaphore/semaphore.go;l=66,
			// which suggests that if the context is already done, `Acquire()` may still succeed without blocking. This introduces
			// unpredictability in my test cases, so in order to avoid that, I am adding this cancellation check.
			select {
			case <-ctx.Done():
				log.Errorf("Context canceled or timed out during %q restore", fe.Name)
				rec.RecordError(vterrors.Errorf(vtrpc.Code_CANCELED, "context canceled"))
				return
			default:
			}

			if rec.HasErrors() {
				params.Logger.Infof("Failed to restore files due to error.")
				return
			}

			fe.ParentPath = createdDir
			// And restore the file.
			name := fmt.Sprintf("%v", i)
			params.Logger.Infof("Copying file %v: %v", name, fe.Name)
			err := be.restoreFile(ctx, params, bh, fe, bm, name)
			if err != nil {
				rec.RecordError(vterrors.Wrapf(err, "can't restore file %v to %v", name, fe.Name))
			}
		}(i)
	}
	wg.Wait()
	return createdDir, rec.Error()
}

// restoreFile restores an individual file.
func (be *BuiltinBackupEngine) restoreFile(ctx context.Context, params RestoreParams, bh backupstorage.BackupHandle, fe *FileEntry, bm builtinBackupManifest, name string) (finalErr error) {
	// Open the source file for reading.
	openSourceAt := time.Now()
	source, err := bh.ReadFile(ctx, name)
	if err != nil {
		return vterrors.Wrap(err, "can't open source file for reading")
	}
	params.Stats.Scope(stats.Operation("Source:Open")).TimedIncrement(time.Since(openSourceAt))

	readStats := params.Stats.Scope(stats.Operation("Source:Read"))
	timedSource := ioutil.NewMeteredReader(source, readStats.TimedIncrementBytes)

	defer func() {
		closeSourceAt := time.Now()
		source.Close()
		params.Stats.Scope(stats.Operation("Source:Close")).TimedIncrement(time.Since(closeSourceAt))
	}()

	br := newBackupReader(name, 0, timedSource)
	go br.ReportProgress(builtinBackupProgress, params.Logger)
	var reader io.Reader = br

	// Open the destination file for writing.
	openDestAt := time.Now()
	dest, err := fe.open(params.Cnf, false)
	if err != nil {
		return vterrors.Wrap(err, "can't open destination file for writing")
	}
	params.Stats.Scope(stats.Operation("Destination:Open")).TimedIncrement(time.Since(openDestAt))

	defer func() {
		closeDestAt := time.Now()
		if cerr := dest.Close(); cerr != nil {
			if finalErr != nil {
				// We already have an error, just log this one.
				log.Errorf("failed to close file %v: %v", name, cerr)
			} else {
				finalErr = vterrors.Wrap(cerr, "failed to close destination file")
			}
		}
		params.Stats.Scope(stats.Operation("Destination:Close")).TimedIncrement(time.Since(closeDestAt))
	}()

	writeStats := params.Stats.Scope(stats.Operation("Destination:Write"))
	timedDest := ioutil.NewMeteredWriter(dest, writeStats.TimedIncrementBytes)

	bufferedDest := bufio.NewWriterSize(timedDest, int(builtinBackupFileWriteBufferSize))

	// Create the uncompresser if needed.
	if !bm.SkipCompress {
		var decompressor io.ReadCloser
		var deCompressionEngine = bm.CompressionEngine

		if deCompressionEngine == "" {
			// for backward compatibility
			deCompressionEngine = PgzipCompressor
		}
		externalDecompressorCmd := ExternalDecompressorCmd
		if externalDecompressorCmd == "" && bm.ExternalDecompressor != "" {
			externalDecompressorCmd = bm.ExternalDecompressor
		}
		if externalDecompressorCmd != "" {
			if deCompressionEngine == ExternalCompressor {
				deCompressionEngine = externalDecompressorCmd
				decompressor, err = newExternalDecompressor(ctx, deCompressionEngine, reader, params.Logger)
			} else {
				decompressor, err = newBuiltinDecompressor(deCompressionEngine, reader, params.Logger)
			}
		} else {
			if deCompressionEngine == ExternalCompressor {
				return fmt.Errorf("%w value: %q", errUnsupportedDeCompressionEngine, ExternalCompressor)
			}
			decompressor, err = newBuiltinDecompressor(deCompressionEngine, reader, params.Logger)
		}
		if err != nil {
			return vterrors.Wrap(err, "can't create decompressor")
		}

		decompressStats := params.Stats.Scope(stats.Operation("Decompressor:Read"))
		reader = ioutil.NewMeteredReader(decompressor, decompressStats.TimedIncrementBytes)

		defer func() {
			closeDecompressorAt := time.Now()
			if cerr := decompressor.Close(); cerr != nil {
				params.Logger.Errorf("failed to close decompressor: %v", cerr)
				if finalErr != nil {
					// We already have an error, just log this one.
					log.Errorf("failed to close decompressor %v: %v", name, cerr)
				} else {
					finalErr = vterrors.Wrap(cerr, "failed to close decompressor")
				}
			}
			params.Stats.Scope(stats.Operation("Decompressor:Close")).TimedIncrement(time.Since(closeDecompressorAt))
		}()
	}

	// Copy the data. Will also write to the hasher.
	if _, err = io.Copy(bufferedDest, reader); err != nil {
		return vterrors.Wrap(err, "failed to copy file contents")
	}

	// Check the hash.
	hash := br.HashString()
	if hash != fe.Hash {
		return vterrors.Errorf(vtrpc.Code_INTERNAL, "hash mismatch for %v, got %v expected %v", fe.Name, hash, fe.Hash)
	}

	// Flush the buffer.
	closeDestAt := time.Now()
	if err := bufferedDest.Flush(); err != nil {
		return vterrors.Wrap(err, "failed to flush destination buffer")
	}
	params.Stats.Scope(stats.Operation("Destination:Close")).TimedIncrement(time.Since(closeDestAt))

	closeSourceAt := time.Now()
	if err := br.Close(); err != nil {
		return vterrors.Wrap(err, "failed to close the source reader")
	}
	params.Stats.Scope(stats.Operation("Source:Close")).TimedIncrement(time.Since(closeSourceAt))

	return nil
}

// ShouldDrainForBackup satisfies the BackupEngine interface
// backup requires query service to be stopped, hence true
func (be *BuiltinBackupEngine) ShouldDrainForBackup() bool {
	return true
}

func getPrimaryPosition(ctx context.Context, tmc tmclient.TabletManagerClient, ts *topo.Server, keyspace, shard string) (mysql.Position, error) {
	si, err := ts.GetShard(ctx, keyspace, shard)
	if err != nil {
		return mysql.Position{}, vterrors.Wrap(err, "can't read shard")
	}
	if topoproto.TabletAliasIsZero(si.PrimaryAlias) {
		return mysql.Position{}, fmt.Errorf("shard %v/%v has no primary", keyspace, shard)
	}
	ti, err := ts.GetTablet(ctx, si.PrimaryAlias)
	if err != nil {
		return mysql.Position{}, fmt.Errorf("can't get primary tablet record %v: %v", topoproto.TabletAliasString(si.PrimaryAlias), err)
	}
	posStr, err := tmc.PrimaryPosition(ctx, ti.Tablet)
	if err != nil {
		return mysql.Position{}, fmt.Errorf("can't get primary replication position: %v", err)
	}
	pos, err := mysql.DecodePosition(posStr)
	if err != nil {
		return mysql.Position{}, fmt.Errorf("can't decode primary replication position %q: %v", posStr, err)
	}
	return pos, nil
}

func init() {
	BackupRestoreEngineMap["builtin"] = &BuiltinBackupEngine{}
}
