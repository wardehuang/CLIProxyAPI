package main

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestScheduleGroupReleaseUsesRequestIDWhenSelectedAuthIDIsMissing(t *testing.T) {
	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory database: %v", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)

	_, err = database.Exec(`CREATE TABLE plugin_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at INTEGER NOT NULL,
		level TEXT NOT NULL,
		event TEXT NOT NULL,
		message TEXT NOT NULL,
		detail TEXT NOT NULL,
		category TEXT NOT NULL,
		group_id TEXT NOT NULL,
		log_status TEXT NOT NULL,
		node_id INTEGER NOT NULL,
		node_name TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create plugin log table: %v", err)
	}

	state := newScheduleGroupState()
	state.busyByGroup[2] = "auth-2"
	state.groupByAuth["auth-2"] = 2
	state.requestIDByGroup[2] = "request-1"
	store := &guardianStore{database: database}

	state.release(store, pluginapi.RequestCompletion{
		RequestID:  "request-1",
		Outcome:    pluginapi.RequestCompletionSucceeded,
		StatusCode: 200,
	})

	if state.hasBusy() {
		t.Fatal("schedule group remained busy after matching request completion")
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()
	if len(state.groupByAuth) != 0 || len(state.requestIDByGroup) != 0 {
		t.Fatalf("schedule group mappings remain: auth=%v request=%v", state.groupByAuth, state.requestIDByGroup)
	}

	rows, err := database.Query("SELECT event, detail FROM plugin_logs ORDER BY id")
	if err != nil {
		t.Fatalf("query plugin logs: %v", err)
	}
	defer rows.Close()
	var mismatchLogged, releaseLogged bool
	for rows.Next() {
		var event, detail string
		if err := rows.Scan(&event, &detail); err != nil {
			t.Fatalf("scan plugin log: %v", err)
		}
		if event == "schedule.group_release_mismatch" {
			mismatchLogged = strings.Contains(detail, "reason=selected_auth_id_missing released=true")
		}
		if event == "schedule.group_released" {
			releaseLogged = strings.Contains(detail, "request_id_match=true") && strings.Contains(detail, "release_method=request_id")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plugin logs: %v", err)
	}
	if !mismatchLogged || !releaseLogged {
		t.Fatalf("expected mismatch and request-ID release logs; mismatch=%t release=%t", mismatchLogged, releaseLogged)
	}
}
