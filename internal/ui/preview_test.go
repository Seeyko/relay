package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/store"

	_ "github.com/Seeyko/relay/internal/providers/meta"
	_ "github.com/Seeyko/relay/internal/providers/x"
	_ "github.com/Seeyko/relay/internal/providers/youtube"
)

func TestDraftsAndPreview(t *testing.T) {
	h := newHarness(t)
	loc, _ := time.LoadLocation("Europe/Paris")
	h.ui.Svc = &core.Service{Store: h.st, Location: loc}
	ctx := t.Context()

	// Seed: one draft across X and Instagram (planned), one undated draft, one scheduled post.
	x := &store.Account{Provider: "x", Name: "RMP", Handle: "rmp"}
	ig := &store.Account{Provider: "instagram", Name: "RMP IG", Handle: "rmp.ig"}
	for _, a := range []*store.Account{x, ig} {
		if err := h.st.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	img := &store.Media{Filename: "spot.jpg", Mime: "image/jpeg", Size: 1200, Path: "s3:med_img.jpg"} // in a private bucket
	vid := &store.Media{Filename: "clip.mp4", Mime: "video/mp4", Size: 5000, Path: "/data/media/med_vid.mp4"}
	for _, m := range []*store.Media{img, vid} {
		if err := h.st.CreateMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	planned := time.Date(2026, 1, 5, 17, 0, 0, 0, time.UTC)
	long := strings.Repeat("a", 300)
	gx := &store.Post{GroupID: "grp_launch", AccountID: x.ID, Status: store.StatusDraft, Parts: []string{long, "second part"}, Media: []string{img.ID}, PlannedAt: &planned, Options: map[string]any{"kind": "thread"}}
	gi := &store.Post{GroupID: "grp_launch", AccountID: ig.ID, Status: store.StatusDraft, Parts: []string{"Caption #skate"}, Media: []string{img.ID, vid.ID}, PlannedAt: &planned, Options: map[string]any{"kind": "carousel", "alt": "a skatepark"}}
	undated := &store.Post{GroupID: "grp_idea", AccountID: x.ID, Status: store.StatusDraft, Parts: []string{"idea"}, CreatedAt: time.Now().UTC().Add(time.Minute)}
	sched := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)
	sp := &store.Post{GroupID: "grp_sched", AccountID: x.ID, Status: store.StatusScheduled, Parts: []string{"scheduled"}, ScheduledAt: &sched}
	for _, p := range []*store.Post{gx, gi, undated, sp} {
		if err := h.st.CreatePost(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	if code, _ := h.do("GET", "/ui/api/drafts", nil, nil); code != 401 {
		t.Fatalf("drafts without session: want 401, got %d", code)
	}
	if code, _ := h.do("GET", "/ui/api/posts/"+gx.ID, nil, nil); code != 401 {
		t.Fatalf("preview without session: want 401, got %d", code)
	}
	k := newSoftKey("localhost", h.origin)
	h.register(k)
	if code, res := h.login(k); code != 200 {
		t.Fatalf("login: %d %v", code, res)
	}

	// Calendar: the planned draft sits at its planned time; the undated draft is not in the calendar.
	_, cal := h.do("GET", "/ui/api/calendar?from=2026-01-05T00:00:00Z&to=2026-01-12T00:00:00Z", nil, nil)
	ids := map[string]map[string]any{}
	for _, p := range cal["posts"].([]any) {
		m := p.(map[string]any)
		ids[m["id"].(string)] = m
	}
	if m := ids[gx.ID]; m == nil || m["planned"] != true || m["at"] != "2026-01-05T17:00:00Z" || m["status"] != "draft" {
		t.Fatalf("planned draft in calendar: %v", cal)
	}
	if ids[sp.ID] == nil || ids[sp.ID]["planned"] != nil {
		t.Fatalf("scheduled post in calendar: %v", cal)
	}
	_, now := h.do("GET", "/ui/api/calendar?from="+time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339)+"&to="+time.Now().UTC().Add(24*time.Hour).Format(time.RFC3339), nil, nil)
	for _, p := range now["posts"].([]any) {
		if p.(map[string]any)["id"] == undated.ID {
			t.Fatal("an undated draft must not show in the calendar")
		}
	}

	// Drafts: one card per group, newest first, every network of the group, problems counted.
	code, dr := h.do("GET", "/ui/api/drafts", nil, nil)
	if code != 200 {
		t.Fatalf("drafts: %d %v", code, dr)
	}
	groups := dr["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("want 2 draft groups, got %v", dr)
	}
	first, second := groups[0].(map[string]any), groups[1].(map[string]any)
	if first["group_id"] != "grp_idea" || second["group_id"] != "grp_launch" || second["planned_at"] != "2026-01-05T17:00:00Z" {
		t.Fatalf("draft order / planned_at: %v", dr)
	}
	launch := second["posts"].([]any)
	if len(launch) != 2 {
		t.Fatalf("launch group should have 2 networks: %v", second)
	}
	byNet := map[string]map[string]any{}
	for _, p := range launch {
		m := p.(map[string]any)
		byNet[m["provider"].(string)] = m
	}
	if byNet["x"]["problems"].(float64) < 1 || byNet["x"]["media"].(map[string]any)["url"] != "/media/med_img.jpg" {
		t.Fatalf("x draft summary: %v", byNet["x"])
	}

	// Preview: the whole group, requested post first, full text, counters, media, options, problems.
	code, pv := h.do("GET", "/ui/api/posts/"+gi.ID, nil, nil)
	if code != 200 {
		t.Fatalf("preview: %d %v", code, pv)
	}
	if pv["timezone"] != "Europe/Paris" || pv["group_id"] != "grp_launch" {
		t.Fatalf("preview header: %v", pv)
	}
	posts := pv["posts"].([]any)
	if len(posts) != 2 {
		t.Fatalf("preview should hold both networks: %v", pv)
	}
	igp := posts[0].(map[string]any)
	if igp["post"].(map[string]any)["id"] != gi.ID || igp["layout"] != "caption" || igp["account"].(map[string]any)["handle"] != "rmp.ig" {
		t.Fatalf("instagram preview: %v", igp)
	}
	if _, leak := igp["account"].(map[string]any)["credentials"]; leak {
		t.Fatal("credentials must never reach the dashboard")
	}
	cap := igp["texts"].([]any)[0].(map[string]any)
	if cap["label"] != "caption" || cap["limit"].(float64) != 2200 || cap["fold"].(float64) != 125 || cap["count"].(float64) != 14 {
		t.Fatalf("instagram caption: %v", cap)
	}
	media := igp["media"].([]any)
	if len(media) != 2 || media[0].(map[string]any)["path"] != "/media/med_img.jpg" || media[1].(map[string]any)["mime"] != "video/mp4" {
		t.Fatalf("instagram media (order matters): %v", media)
	}
	if igp["post"].(map[string]any)["options"].(map[string]any)["alt"] != "a skatepark" {
		t.Fatalf("options should be in the preview: %v", igp["post"])
	}
	xp := posts[1].(map[string]any)
	texts := xp["texts"].([]any)
	if xp["layout"] != "thread" || len(texts) != 2 || texts[0].(map[string]any)["count"].(float64) != 300 || texts[0].(map[string]any)["limit"].(float64) != 280 || texts[1].(map[string]any)["text"] != "second part" {
		t.Fatalf("x thread preview: %v", xp)
	}
	probs := xp["problems"].([]any)
	if len(probs) == 0 || !strings.Contains(probs[0].(string), "280") {
		t.Fatalf("x pre-flight problems should be reported: %v", probs)
	}

	if code, _ := h.do("GET", "/ui/api/posts/post_missing", nil, nil); code != 404 {
		t.Fatalf("unknown post: want 404, got %d", code)
	}
}
