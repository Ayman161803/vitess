/*
Copyright 2020 The Vitess Authors.

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

package schema

import (
	"testing"

	"github.com/stretchr/testify/require"

	"context"

	"vitess.io/vitess/go/sqltypes"
	binlogdatapb "vitess.io/vitess/go/vt/proto/binlogdata"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/tabletenv"
)

func TestTracker(t *testing.T) {
	initialSchemaInserted := false
	se, db, cancel := getTestSchemaEngine(t, 0)
	defer cancel()
	gtid1 := "MySQL56/7b04699f-f5e9-11e9-bf88-9cb6d089e1c3:1-10"
	ddl1 := "create table tracker_test (id int)"
	query := "CREATE TABLE IF NOT EXISTS _vt.schema_version.*"
	db.AddQueryPattern(query, &sqltypes.Result{})

	db.AddQueryPattern("insert into _vt.schema_version.*1-10.*", &sqltypes.Result{})
	db.AddQueryPatternWithCallback("insert into _vt.schema_version.*1-3.*", &sqltypes.Result{}, func(query string) {
		initialSchemaInserted = true
	})
	// simulates empty schema_version table, so initial schema should be inserted
	db.AddQuery("select id from _vt.schema_version limit 1", &sqltypes.Result{Rows: [][]sqltypes.Value{}})
	// called to get current position
	db.AddQuery("SELECT @@GLOBAL.gtid_executed", sqltypes.MakeTestResult(sqltypes.MakeTestFields(
		"",
		"varchar"),
		"7b04699f-f5e9-11e9-bf88-9cb6d089e1c3:1-3",
	))
	vs := &fakeVstreamer{
		done: make(chan struct{}),
		events: [][]*binlogdatapb.VEvent{{
			{
				Type: binlogdatapb.VEventType_GTID,
				Gtid: gtid1,
			}, {
				Type:      binlogdatapb.VEventType_DDL,
				Statement: ddl1,
			},
			{
				Type:      binlogdatapb.VEventType_GTID,
				Statement: "", // This event should cause an error updating schema since gtid is bad
			}, {
				Type:      binlogdatapb.VEventType_DDL,
				Statement: ddl1,
			},
			{
				Type: binlogdatapb.VEventType_GTID,
				Gtid: gtid1,
			}, {
				Type:      binlogdatapb.VEventType_DDL,
				Statement: "",
			},
		}},
	}
	config := se.env.Config()
	config.TrackSchemaVersions = true
	env := tabletenv.NewEnv(config, "TrackerTest")
	initial := env.Stats().ErrorCounters.Counts()["INTERNAL"]
	tracker := NewTracker(env, vs, se)
	tracker.Open()
	<-vs.done
	cancel()
	tracker.Close()
	final := env.Stats().ErrorCounters.Counts()["INTERNAL"]
	require.Equal(t, initial+1, final)
	require.True(t, initialSchemaInserted)
}

func TestTrackerShouldNotInsertInitialSchema(t *testing.T) {
	initialSchemaInserted := false
	se, db, cancel := getTestSchemaEngine(t, 0)
	gtid1 := "MySQL56/7b04699f-f5e9-11e9-bf88-9cb6d089e1c3:1-10"

	defer cancel()
	// simulates existing rows in schema_version, so initial schema should not be inserted
	db.AddQuery("select id from _vt.schema_version limit 1", sqltypes.MakeTestResult(sqltypes.MakeTestFields(
		"id",
		"int"),
		"1",
	))
	// called to get current position
	db.AddQuery("SELECT @@GLOBAL.gtid_executed", sqltypes.MakeTestResult(sqltypes.MakeTestFields(
		"",
		"varchar"),
		"7b04699f-f5e9-11e9-bf88-9cb6d089e1c3:1-3",
	))
	db.AddQueryPatternWithCallback("insert into _vt.schema_version.*1-3.*", &sqltypes.Result{}, func(query string) {
		initialSchemaInserted = true
	})
	vs := &fakeVstreamer{
		done: make(chan struct{}),
		events: [][]*binlogdatapb.VEvent{{
			{
				Type: binlogdatapb.VEventType_GTID,
				Gtid: gtid1,
			},
		}},
	}
	config := se.env.Config()
	config.TrackSchemaVersions = true
	env := tabletenv.NewEnv(config, "TrackerTest")
	tracker := NewTracker(env, vs, se)
	tracker.Open()
	<-vs.done
	cancel()
	tracker.Close()
	require.False(t, initialSchemaInserted)
}

var _ VStreamer = (*fakeVstreamer)(nil)

type fakeVstreamer struct {
	done   chan struct{}
	events [][]*binlogdatapb.VEvent
}

func (f *fakeVstreamer) Stream(ctx context.Context, startPos string, tablePKs []*binlogdatapb.TableLastPK, filter *binlogdatapb.Filter, send func([]*binlogdatapb.VEvent) error) error {
	for _, events := range f.events {
		err := send(events)
		if err != nil {
			return err
		}
	}
	close(f.done)
	<-ctx.Done()
	return nil
}

func TestMustReloadSchemaOnDDL(t *testing.T) {
	type testcase struct {
		query  string
		dbname string
		want   bool
	}
	db1, db2 := "db1", "db2"
	testcases := []*testcase{
		{"create table x(i int);", db1, true},
		{"bad", db2, false},
		{"create table db2.x(i int);", db2, true},
		{"rename table db2.x to db2.y;", db2, true},
		{"create table db1.x(i int);", db2, false},
		{"create table _vt.x(i int);", db1, false},
		{"DROP VIEW IF EXISTS `pseudo_gtid`.`_pseudo_gtid_hint__asc:55B364E3:0000000000056EE2:6DD57B85`", db2, false},
		{"create database db1;", db1, false},
		{"create table db1._4e5dcf80_354b_11eb_82cd_f875a4d24e90_20201203114014_gho(i int);", db1, false},
	}
	for _, tc := range testcases {
		t.Run("", func(t *testing.T) {
			require.Equal(t, tc.want, MustReloadSchemaOnDDL(tc.query, tc.dbname))
		})
	}
}
