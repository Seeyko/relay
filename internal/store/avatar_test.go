package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// A database from before profile pictures gets the avatar columns on open; an automatic picture never
// replaces a manual one; a deleted media file leaves the account without a picture, not broken.
func TestAccountAvatar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE accounts (id TEXT PRIMARY KEY, provider TEXT NOT NULL, name TEXT NOT NULL, handle TEXT NOT NULL DEFAULT '', remote_user_id TEXT NOT NULL DEFAULT '', credentials BLOB, created_at TIMESTAMP NOT NULL);
INSERT INTO accounts VALUES ('acc_1','x','A','a','',NULL,'2026-01-01 00:00:00');`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	a, err := st.GetAccount(ctx, "acc_1")
	if err != nil || a.Avatar != "" || a.AvatarMediaID != "" || a.AvatarSource != "" || a.AvatarCheckedAt != nil {
		t.Fatalf("old account: %+v %v", a, err)
	}

	m := &Media{Filename: "a.jpg", Mime: "image/jpeg", Size: 1, Path: "/data/media/med_1.jpg"}
	if err := st.CreateMedia(ctx, m); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)
	if ok, err := st.SetAccountAvatar(ctx, "acc_1", m.ID, AvatarManual, &now); !ok || err != nil {
		t.Fatalf("manual: %v %v", ok, err)
	}
	if ok, err := st.SetAccountAvatar(ctx, "acc_1", "med_other", AvatarAuto, &now); ok || err != nil {
		t.Fatalf("an automatic picture replaced a manual one: %v %v", ok, err)
	}
	accs, err := st.ListAccounts(ctx)
	if err != nil || len(accs) != 1 || accs[0].Avatar != "/media/med_1.jpg" || accs[0].AvatarSource != AvatarManual || !accs[0].AvatarCheckedAt.Equal(now) {
		t.Fatalf("list: %+v %v", accs, err)
	}
	if used, err := st.MediaInUse(ctx, m.ID); !used || err != nil {
		t.Fatalf("in use: %v %v", used, err)
	}
	if err := st.DeleteMedia(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.GetAccount(ctx, "acc_1"); a.Avatar != "" || a.AvatarMediaID != m.ID {
		t.Fatalf("deleted media: %+v", a)
	}
}
