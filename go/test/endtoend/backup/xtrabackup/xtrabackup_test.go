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

package vtctlbackup

import (
	"testing"

	"vitess.io/vitess/go/vt/mysqlctl"

	backup "vitess.io/vitess/go/test/endtoend/backup/vtctlbackup"
)

// TestXtraBackup - tests the backup using xtrabackup
func TestXtrabackup(t *testing.T) {
	backup.TestBackup(t, backup.XtraBackup, "tar", 0, nil, nil)
}

func TestXtrabackupWithZstdCompression(t *testing.T) {
	defer setDefaultCompressionFlag()
	cDetails := &backup.CompressionDetails{
		CompressorEngineName:    "zstd",
		ExternalCompressorCmd:   "zstd",
		ExternalCompressorExt:   ".zst",
		ExternalDecompressorCmd: "zstd -d",
	}

	backup.TestBackup(t, backup.XtraBackup, "tar", 0, cDetails, []string{"TestReplicaBackup"})
}

func TestXtrabackupWithExternalZstdCompression(t *testing.T) {
	defer setDefaultCompressionFlag()
	cDetails := &backup.CompressionDetails{
		CompressorEngineName:    "external",
		ExternalCompressorCmd:   "zstd",
		ExternalCompressorExt:   ".zst",
		ExternalDecompressorCmd: "zstd -d",
	}

	backup.TestBackup(t, backup.XtraBackup, "tar", 0, cDetails, []string{"TestReplicaBackup"})
}

func TestXtrabackupWithExternalZstdCompressionAndManifestedDecompressor(t *testing.T) {
	defer setDefaultCompressionFlag()
	cDetails := &backup.CompressionDetails{
		CompressorEngineName:            "external",
		ExternalCompressorCmd:           "zstd",
		ExternalCompressorExt:           ".zst",
		ManifestExternalDecompressorCmd: "zstd -d",
	}

	backup.TestBackup(t, backup.XtraBackup, "tar", 0, cDetails, []string{"TestReplicaBackup"})
}

func setDefaultCompressionFlag() {
	mysqlctl.CompressionEngineName = "pgzip"
	mysqlctl.ExternalCompressorCmd = ""
	mysqlctl.ExternalCompressorExt = ""
	mysqlctl.ExternalDecompressorCmd = ""
	mysqlctl.ManifestExternalDecompressorCmd = ""
}
