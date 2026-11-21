package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A database created before planned_at existed gets the column on open, and its posts keep working.
func TestMigrateAddsPlannedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE accounts (id TEXT PRIMARY KEY, provider TEXT NOT NULL, name TEXT NOT NULL, handle TEXT NOT NULL DEFAULT '', remote_user_id TEXT NOT NULL DEFAULT '', credentials BLOB, created_at TIMESTAMP NOT NULL);
CREATE TABLE posts (id TEXT PRIMARY KEY, group_id TEXT NOT NULL, account_id TEXT NOT NULL, status TEXT NOT NULL, parts TEXT NOT NULL, media TEXT NOT NULL DEFAULT '[]', options TEXT NOT NULL DEFAULT '{}', scheduled_at TIMESTAMP, published_at TIMESTAMP, remote_id TEXT NOT NULL DEFAULT '', remote_ids TEXT NOT NULL DEFAULT '[]', remote_url TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TIMESTAMP, metrics TEXT NOT NULL DEFAULT '{}', metrics_at TIMESTAMP, source TEXT NOT NULL DEFAULT 'relay', created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL);
INSERT INTO accounts VALUES ('acc_1','x','A','a','',NULL,'2026-01-01 00:00:00');
INSERT INTO posts(id,group_id,account_id,status,parts,created_at,updated_at) VALUES ('post_old','post_old','acc_1','draft','["old"]','2026-01-01 00:00:00','2026-01-01 00:00:00');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for range 2 { // opening twice must not try to add the column again
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	p, err := st.GetPost(ctx, "post_old")
	if err != nil || p.PlannedAt != nil || p.Parts[0] != "old" {
		t.Fatalf("old post: %+v %v", p, err)
	}
	at := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)
	p.PlannedAt = &at
	if err := st.UpdatePost(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p, _ = st.GetPost(ctx, "post_old"); p.PlannedAt == nil || !p.PlannedAt.Equal(at) {
		t.Fatalf("planned_at not saved: %+v", p)
	}
}

// part_media: added on open to an old database, kept aligned with parts, counted by MediaInUse.
func TestPartMedia(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// A database from before part_media (planned_at already there).
	if _, err := db.Exec(`CREATE TABLE accounts (id TEXT PRIMARY KEY, provider TEXT NOT NULL, name TEXT NOT NULL, handle TEXT NOT NULL DEFAULT '', remote_user_id TEXT NOT NULL DEFAULT '', credentials BLOB, created_at TIMESTAMP NOT NULL);
CREATE TABLE posts (id TEXT PRIMARY KEY, group_id TEXT NOT NULL, account_id TEXT NOT NULL, status TEXT NOT NULL, parts TEXT NOT NULL, media TEXT NOT NULL DEFAULT '[]', options TEXT NOT NULL DEFAULT '{}', scheduled_at TIMESTAMP, published_at TIMESTAMP, remote_id TEXT NOT NULL DEFAULT '', remote_ids TEXT NOT NULL DEFAULT '[]', remote_url TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TIMESTAMP, metrics TEXT NOT NULL DEFAULT '{}', metrics_at TIMESTAMP, source TEXT NOT NULL DEFAULT 'relay', created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL, planned_at TIMESTAMP);
INSERT INTO accounts VALUES ('acc_1','x','A','a','',NULL,'2026-01-01 00:00:00');
INSERT INTO posts(id,group_id,account_id,status,parts,media,created_at,updated_at) VALUES ('post_old','post_old','acc_1','scheduled','["one","two"]','["med_a"]','2026-01-01 00:00:00','2026-01-01 00:00:00');`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	p, err := st.GetPost(ctx, "post_old")
	if err != nil || p.PartMedia != nil || p.MediaOf(0)[0] != "med_a" || p.MediaOf(1) != nil {
		t.Fatalf("old post: %+v %v", p, err)
	}
	if b, _ := json.Marshal(p); strings.Contains(string(b), "part_media") {
		t.Fatalf("an old post must serialize as before: %s", b)
	}
	if used, _ := st.MediaInUse(ctx, "med_b"); used {
		t.Fatal("med_b is not used yet")
	}
	// Stale [0], a trailing empty part and a part past the end are normalized away.
	p.PartMedia = [][]string{{"stale"}, {"med_b"}, {}, {"med_c"}}
	p.Parts = []string{"one", "two", "three"}
	if err := st.UpdatePost(ctx, p); err != nil {
		t.Fatal(err)
	}
	p, _ = st.GetPost(ctx, "post_old")
	if !reflect.DeepEqual(p.PartMedia, [][]string{{"med_a"}, {"med_b"}}) {
		t.Fatalf("part_media %v", p.PartMedia)
	}
	if used, _ := st.MediaInUse(ctx, "med_b"); !used {
		t.Fatal("a media used by part 2 only must count as in use")
	}
	// Back to a single part: no part media left.
	p.Parts = []string{"one"}
	if err := st.UpdatePost(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p, _ = st.GetPost(ctx, "post_old"); p.PartMedia != nil {
		t.Fatalf("part_media %v", p.PartMedia)
	}
}
